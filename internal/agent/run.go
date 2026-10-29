package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"syscall"
	"time"

	"github.com/webcodingcat/clowder/internal/api"
	"github.com/webcodingcat/clowder/internal/buildinfo"
	"github.com/webcodingcat/clowder/internal/compose"
	"github.com/webcodingcat/clowder/internal/controller"
	"github.com/webcodingcat/clowder/internal/network"
	"github.com/webcodingcat/clowder/internal/raftnode"
	"github.com/webcodingcat/clowder/internal/state"
	"github.com/webcodingcat/clowder/internal/supervisor"
)

const (
	// shutdownTimeout is how long open API calls get when the agent stops.
	shutdownTimeout = 5 * time.Second
	// catchUpTimeout is how long the agent waits at start for its state to
	// catch up with the Raft log; well within systemd's start timeout.
	catchUpTimeout = 15 * time.Second
	// reconcileInterval is how often the reconcilers (network, stacks) run
	// without a change of the state, to repair what someone changed by hand.
	reconcileInterval = 30 * time.Second
)

// HostParts are what the agent changes on its host, and the hoster's API
// it calls while it leads; tests use fakes.
type HostParts struct {
	Network HostNetwork
	Storage Storage
	Docker  Docker
	Hoster  controller.ConnectHoster
}

// Run is the agent until ctx ends: clowder0, the Raft member, the API, the
// containers' DNS resolver, the nft rules, the stacks of this host with
// their volumes and, while it leads, the placement of stacks and the
// Floating IP. It tells systemd when it serves and keeps its watchdog fed
// while the state answers.
func Run(ctx context.Context, files Files, parts HostParts, logger *slog.Logger) error {
	if files.SecretsDir == "" {
		return errors.New("run needs a directory for the stacks' secrets")
	}
	host, err := files.readHost()
	if err != nil {
		return err
	}
	credentials, err := files.readCredentials()
	if err != nil {
		return err
	}
	if err := parts.Network.SetUpInterface(ctx, host.NodeAddress); err != nil {
		return err
	}
	node, err := raftnode.Open(raftnode.Config{
		Dir: files.raftDir(), ID: host.ID, Bind: host.raftAddress(), Advertise: host.raftAddress(),
		Certificate: credentials.certificate, CA: credentials.ca, Logger: logger,
	})
	if err != nil {
		return err
	}
	defer func() { _ = node.Close() }() // the log is on disk; nothing is lost

	// answer from the state as the log has it, not from an empty one; a host
	// without quorum starts anyway after a while, and says so
	catchUp, cancelCatchUp := context.WithTimeout(ctx, catchUpTimeout)
	caughtUp := node.CatchUp(catchUp)
	cancelCatchUp()
	if caughtUp != nil {
		logger.Warn("serving before the state caught up with the Raft log", "error", caughtUp)
	}

	resolver, err := parts.Network.StartResolver()
	if err != nil {
		return err
	}
	defer func() { _ = resolver.Close() }() // stopping: nothing to keep
	reconcilers, stopReconcilers := context.WithCancel(ctx)
	defer stopReconcilers()
	go reconcileNetwork(reconcilers, node, parts.Network, logger)

	digestKey, err := credentials.dataKey.Derive(secretDigestPurpose)
	if err != nil {
		return err
	}
	runner := &stackRunner{
		files: files, host: host, version: buildinfo.Version, cluster: node,
		docker: parts.Docker, storage: parts.Storage, open: credentials.dataKey.Open, digestKey: digestKey,
		logger: logger, applied: map[string][32]byte{}, logins: map[string][32]byte{}, wake: make(chan struct{}, 1),
	}
	server := api.NewServer(api.ServerConfig{
		Cluster: node, CA: credentials.ca, Certificate: credentials.certificate,
		APIAddress: func(id state.HostID) string {
			// every host has the same API port; tests on 127.0.0.1 have one host
			return net.JoinHostPort(network.NodeAddress(id).String(), strconv.Itoa(host.APIPort))
		},
		Keys: credentials.dataKey, Version: buildinfo.Version, Logger: logger,
		HostID: host.ID, Containers: runner,
	})
	runner.propose = server.Propose
	runner.supervisor = supervisor.New(parts.Docker, runner.rules, supervisor.DefaultBackoff, runner.forgetApplied, logger)
	go runner.supervisor.Run(reconcilers, compose.LabelStack)
	go controller.RunPlacement(reconcilers, node, server.Propose, logger)
	go controller.RunIngress(reconcilers, node, server.Propose, credentials.dataKey.Open, parts.Hoster, logger)
	go runner.trim(reconcilers)
	go func() {
		// a state that lags may still place a stack here that has moved on,
		// so containers start only once the state caught up
		if caughtUp != nil && !waitForState(reconcilers, node, logger) {
			return
		}
		runner.reconcile(reconcilers)
	}()
	servers, serveErrors, err := serveAPI(server, host.apiAddresses(), logger)
	if err != nil {
		return err
	}
	logger.Info("serving", "host", host.Name, "api", host.apiAddresses(), "raft", host.raftAddress())
	if err := notify("READY=1"); err != nil {
		logger.Warn("systemd", "error", err)
	}

	err = watch(ctx, node, serveErrors, logger)
	shutdown, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	for _, server := range servers {
		_ = server.Shutdown(shutdown) // stopping: open calls are cut after the timeout
	}
	return err
}

// waitForState tries until ctx ends to catch the state up with the Raft
// log; it tells whether it did.
func waitForState(ctx context.Context, node *raftnode.Node, logger *slog.Logger) bool {
	for ctx.Err() == nil {
		catchUp, cancel := context.WithTimeout(ctx, catchUpTimeout)
		err := node.CatchUp(catchUp)
		cancel()
		if err == nil {
			logger.Info("the state caught up; starting the stacks")
			return true
		}
		// a failed barrier returns at once; do not spin
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
	return false
}

// freeBind lets a socket bind an address the host does not have yet: at
// boot the public address can come up after the agent (DHCP, netplan), and
// the agent must neither fail nor leave users locked out then.
var freeBind = net.ListenConfig{
	Control: func(_, _ string, raw syscall.RawConn) error {
		var optErr error
		err := raw.Control(func(fd uintptr) {
			optErr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_FREEBIND, 1)
		})
		return errors.Join(err, optErr)
	},
}

// serveAPI listens on every address and serves the API there.
func serveAPI(server *api.Server, addresses []string, logger *slog.Logger) ([]*http.Server, <-chan error, error) {
	var servers []*http.Server
	serveErrors := make(chan error, len(addresses))
	for _, address := range addresses {
		listener, err := freeBind.Listen(context.Background(), "tcp", address)
		if err != nil {
			for _, started := range servers {
				_ = started.Close() // the listen error is the one to report
			}
			return nil, nil, fmt.Errorf("listen for the API on %s: %w", address, err)
		}
		httpServer := &http.Server{
			Handler:           server,
			TLSConfig:         server.TLSConfig(),
			ReadHeaderTimeout: 10 * time.Second,
			ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelDebug),
		}
		servers = append(servers, httpServer)
		go func() {
			if err := httpServer.ServeTLS(listener, "", ""); !errors.Is(err, http.ErrServerClosed) {
				serveErrors <- fmt.Errorf("serve the API on %s: %w", address, err)
			}
		}()
	}
	return servers, serveErrors, nil
}

// watch runs until ctx ends or the API fails, and feeds systemd's watchdog
// while the state answers: a deadlocked agent stops feeding it and systemd
// restarts it.
func watch(ctx context.Context, node *raftnode.Node, serveErrors <-chan error, logger *slog.Logger) error {
	interval := watchdogInterval()
	var tick <-chan time.Time
	if interval > 0 {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		tick = ticker.C
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-serveErrors:
			return err
		case <-tick:
			node.Read(func(*state.State) {}) // hangs if the state's lock does
			if err := notify("WATCHDOG=1"); err != nil {
				logger.Warn("systemd", "error", err)
			}
		}
	}
}
