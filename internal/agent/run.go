package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"time"

	"github.com/webcodingcat/clowder/internal/api"
	"github.com/webcodingcat/clowder/internal/buildinfo"
	"github.com/webcodingcat/clowder/internal/network"
	"github.com/webcodingcat/clowder/internal/raftnode"
	"github.com/webcodingcat/clowder/internal/state"
)

const (
	// shutdownTimeout is how long open API calls get when the agent stops.
	shutdownTimeout = 5 * time.Second
	// catchUpTimeout is how long the agent waits at start for its state to
	// catch up with the Raft log; well within systemd's start timeout.
	catchUpTimeout = 15 * time.Second
)

// Run is the agent until ctx ends: the node address, the Raft member and the
// API. It tells systemd when it serves and keeps its watchdog fed while the
// state answers. Containers, network and storage come in later steps.
func Run(ctx context.Context, files Files, ensureNodeAddress func(context.Context, netip.Addr) error,
	logger *slog.Logger,
) error {
	host, err := files.readHost()
	if err != nil {
		return err
	}
	credentials, err := files.readCredentials()
	if err != nil {
		return err
	}
	if err := ensureNodeAddress(ctx, host.NodeAddress); err != nil {
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
	err = node.CatchUp(catchUp)
	cancelCatchUp()
	if err != nil {
		logger.Warn("serving before the state caught up with the Raft log", "error", err)
	}

	server := api.NewServer(api.ServerConfig{
		Cluster: node, CA: credentials.ca, Certificate: credentials.certificate,
		APIAddress: func(id state.HostID) string {
			// every host has the same API port; tests on 127.0.0.1 have one host
			return net.JoinHostPort(network.NodeAddress(id).String(), strconv.Itoa(host.APIPort))
		},
		Keys: credentials.dataKey, Version: buildinfo.Version, Logger: logger,
	})
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

// serveAPI listens on every address and serves the API there.
func serveAPI(server *api.Server, addresses []string, logger *slog.Logger) ([]*http.Server, <-chan error, error) {
	var servers []*http.Server
	serveErrors := make(chan error, len(addresses))
	for _, address := range addresses {
		listener, err := net.Listen("tcp", address)
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
