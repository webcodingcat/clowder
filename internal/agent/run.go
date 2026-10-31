package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
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
	// loopsTimeout is how long the agent waits for its loops when it stops;
	// well within systemd's stop timeout.
	loopsTimeout = 30 * time.Second
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
// their volumes, Traefik with the answers to ACME's challenges and, while
// it leads, the placement of stacks, the Floating IP and the certificates.
// It tells systemd when it serves and keeps its watchdog fed while the
// state answers.
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
	node, caughtUp, err := openNode(ctx, files, host, credentials, logger)
	if err != nil {
		return err
	}
	defer func() { _ = node.Close() }() // the log is on disk; nothing is lost
	resolver, err := parts.Network.StartResolver()
	if err != nil {
		return err
	}
	defer func() { _ = resolver.Close() }() // stopping: nothing to keep

	running, err := newWorkers(files, host, credentials, node, parts, logger)
	if err != nil {
		return err
	}
	reconcilers, stopReconcilers := context.WithCancel(ctx)
	// deferred after the resolver and the Raft node, so it runs before they
	// close: no loop uses them any more then
	var loops sync.WaitGroup
	defer stopLoops(stopReconcilers, &loops, loopsTimeout, logger)
	running.startLoops(reconcilers, &loops, caughtUp)
	servers, serveErrors, err := running.serve()
	if err != nil {
		return err
	}
	logger.Info("serving", "host", host.Name, "api", host.apiAddresses(), "raft", host.raftAddress(),
		"metrics", metricsAddress(host))
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

// openNode opens this host's Raft member and waits a while for its state to
// catch up with the log, so the agent answers from the state as the log has
// it, not from an empty one. A host without quorum starts anyway after
// catchUpTimeout, and says so; caughtUp is why it did not catch up, if so.
func openNode(ctx context.Context, files Files, host Host, credentials credentials, logger *slog.Logger) (
	node *raftnode.Node, caughtUp error, err error,
) {
	node, err = raftnode.Open(raftnode.Config{
		Dir: files.raftDir(), ID: host.ID, Bind: host.raftAddress(), Advertise: host.raftAddress(),
		Certificate: credentials.certificate, CA: credentials.ca, Logger: logger,
	})
	if err != nil {
		return nil, nil, err
	}
	catchUp, cancel := context.WithTimeout(ctx, catchUpTimeout)
	defer cancel()
	if caughtUp = node.CatchUp(catchUp); caughtUp != nil {
		logger.Warn("serving before the state caught up with the Raft log", "error", caughtUp)
	}
	return node, caughtUp, nil
}

// workers are the parts of the agent that run in loops or answer the API,
// and what they share.
type workers struct {
	host        Host
	credentials credentials
	node        *raftnode.Node
	parts       HostParts
	logger      *slog.Logger
	// propose commits through the API server's Propose, for every loop
	propose    func(ctx context.Context, cmd state.Command) (uint64, error)
	runner     *stackRunner
	traefik    *traefikRunner
	conditions *controller.Conditions
	alerts     *controller.Alerts
	server     *api.Server
}

// newWorkers makes the stack runner, Traefik's runner, the alerts, the
// supervisor and the API server; it starts nothing.
func newWorkers(files Files, host Host, credentials credentials, node *raftnode.Node, parts HostParts,
	logger *slog.Logger,
) (*workers, error) {
	digestKey, err := credentials.dataKey.Derive(secretLabelPurpose)
	if err != nil {
		return nil, err
	}
	w := &workers{host: host, credentials: credentials, node: node, parts: parts, logger: logger}
	// The server answers for the stack runner and the alerts (a stack's
	// containers, the test alert, failed deliveries), so it is made after
	// them, and they get its Propose through this. No loop starts before
	// the server is set.
	w.propose = func(ctx context.Context, cmd state.Command) (uint64, error) { return w.server.Propose(ctx, cmd) }
	w.runner = &stackRunner{
		files: files, host: host, cluster: node, propose: w.propose,
		docker: parts.Docker, storage: parts.Storage, open: credentials.dataKey.Open, digestKey: digestKey,
		logger: logger, applied: map[string][32]byte{}, logins: map[string][32]byte{}, wake: make(chan struct{}, 1),
		unexplained: map[string]bool{},
	}
	w.traefik = &traefikRunner{
		files: files, host: host, cluster: node, docker: parts.Docker, open: credentials.dataKey.Open, logger: logger,
		wake: make(chan struct{}, 1), unopened: map[string]bool{},
	}
	w.conditions = controller.NewConditions()
	w.alerts = controller.NewAlerts(node, w.propose, credentials.dataKey.Open, w.conditions, logger)
	w.server = api.NewServer(api.ServerConfig{
		Cluster: node, CA: credentials.ca, Certificate: credentials.certificate,
		APIAddress: func(id state.HostID) string {
			// every host has the same API port; tests on 127.0.0.1 have one host
			return net.JoinHostPort(network.NodeAddress(id).String(), strconv.Itoa(host.APIPort))
		},
		Keys: credentials.dataKey, Version: buildinfo.Version, Logger: logger,
		HostID: host.ID, Containers: w.runner, Alerts: alertsAPI{w.alerts},
	})
	// after Docker restarted, every container is down: the stacks and
	// Traefik come up again
	dockerRestarted := func() {
		w.runner.forgetApplied()
		w.traefik.forget()
	}
	w.runner.supervisor = supervisor.New(parts.Docker, w.runner.rules, supervisor.DefaultBackoff,
		dockerRestarted, w.traefik.look, logger)
	w.traefik.supervisor = w.runner.supervisor
	return w, nil
}

// startLoops starts every loop of the agent; each ends with ctx. caughtUp
// is openNode's: whether the state lagged at the start.
func (w *workers) startLoops(ctx context.Context, loops *sync.WaitGroup, caughtUp error) {
	node, logger, open := w.node, w.logger, w.credentials.dataKey.Open
	loops.Go(func() { reconcileNetwork(ctx, node, w.parts.Network, logger) })
	loops.Go(func() { w.runner.supervisor.Run(ctx, compose.LabelStack) })
	loops.Go(func() { controller.RunPlacement(ctx, node, w.propose, w.conditions, logger) })
	loops.Go(func() { controller.RunIngress(ctx, node, w.propose, open, w.parts.Hoster, w.conditions, logger) })
	loops.Go(func() { w.alerts.Run(ctx) })
	loops.Go(func() { w.alerts.RunHeartbeat(ctx) })
	loops.Go(func() { controller.RunACME(ctx, node, w.propose, w.credentials.dataKey, logger) })
	loops.Go(func() { w.runner.trim(ctx) })
	reporter := &hostReporter{
		host: w.host, version: buildinfo.Version, cluster: node, docker: w.parts.Docker, storage: w.parts.Storage,
		propose: w.propose, logger: logger, root: "/",
	}
	loops.Go(func() {
		// a state that lags may still place a stack here that has moved on,
		// so containers start only once the state caught up: the stacks and
		// Traefik. The report waits too: it compares with the state. Both
		// are loops of their own, started from here, so they wait.
		if caughtUp != nil && !waitForState(ctx, node, logger) {
			return
		}
		loops.Go(func() { reporter.run(ctx) })
		loops.Go(func() { w.traefik.run(ctx) })
		w.runner.reconcile(ctx)
	})
}

// serve starts the API on the host's addresses, the answers to ACME's
// challenges and the metrics. If one cannot listen, it closes those it
// started and returns that error.
func (w *workers) serve() ([]*http.Server, <-chan error, error) {
	servers, serveErrors, err := serveAPI(w.server, w.host.apiAddresses(), w.logger)
	if err != nil {
		return nil, nil, err
	}
	source := &metricsSource{
		host: w.host, cluster: w.node, docker: w.parts.Docker, restarts: w.runner.supervisor.Restarts,
		failures: w.alerts.Failures, memory: readRSS, logger: w.logger,
	}
	for _, start := range []func() (*http.Server, error){
		func() (*http.Server, error) { return serveChallenges(w.node, w.logger) },
		func() (*http.Server, error) { return serveMetrics(source, metricsAddress(w.host), w.logger) },
	} {
		started, err := start()
		if err != nil {
			for _, server := range servers {
				_ = server.Close() // the listen error is the one to report
			}
			return nil, nil, err
		}
		servers = append(servers, started)
	}
	return servers, serveErrors, nil
}

// alertsAPI answers the API's alert routes from the alerts loop.
type alertsAPI struct {
	alerts *controller.Alerts
}

func (a alertsAPI) Test(ctx context.Context) ([]api.AlertTestResult, error) {
	results, err := a.alerts.Test(ctx)
	var answers []api.AlertTestResult
	for _, result := range results {
		answer := api.AlertTestResult{Channel: result.Channel}
		if result.Err != nil {
			answer.Error = result.Err.Error()
		}
		answers = append(answers, answer)
	}
	return answers, err
}

func (a alertsAPI) Failures() []api.AlertFailure {
	answers := []api.AlertFailure{}
	for _, failure := range a.alerts.Failures() {
		answers = append(answers, api.AlertFailure{
			Channel: failure.Channel, Key: failure.Key, Event: failure.Event, Attempts: failure.Attempts,
			LastError: failure.LastError,
		})
	}
	return answers
}

// stopLoops ends the agent's loops and waits for them, at most timeout: a
// pass ends its host tools by their timeouts, and one left behind on a
// suspended DRBD volume does not hold up the stop. It tells whether all
// ended.
func stopLoops(stop context.CancelFunc, loops *sync.WaitGroup, timeout time.Duration, logger *slog.Logger) bool {
	stop()
	ended := make(chan struct{})
	go func() {
		loops.Wait()
		close(ended)
	}()
	select {
	case <-ended:
		return true
	case <-time.After(timeout):
		logger.Warn("stopping before every loop ended", "after", timeout)
		return false
	}
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
			// idle keep-alive connections of CLIs and agents go after a while
			IdleTimeout: 2 * time.Minute,
			ErrorLog:    slog.NewLogLogger(logger.Handler(), slog.LevelDebug),
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
