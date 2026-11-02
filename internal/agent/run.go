package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
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

// HostParts are the host's network, storage and Docker, which the agent
// changes, and how it connects to the hoster's API while it leads; tests
// use fakes.
type HostParts struct {
	Network HostNetwork
	Storage Storage
	Docker  Docker
	Hoster  controller.ConnectHoster
}

// Run runs the agent until ctx ends, in the order docs/developing/host.md
// describes (clowderd run). It tells systemd when it serves and keeps its
// watchdog fed while the state answers.
func Run(ctx context.Context, files Files, parts HostParts, logger *slog.Logger) (err error) {
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
	resolver, err := parts.Network.StartResolver()
	if err != nil {
		_ = node.Close() // the log is on disk; nothing is lost
		return err
	}
	loopsCtx, cancelLoops := context.WithCancel(ctx)
	var loops sync.WaitGroup
	// From here on, every return stops the loops and closes the resolver
	// and the Raft member; with no loop started yet, that is only closing.
	defer func() {
		err = errors.Join(err, stopLoops(cancelLoops, &loops, loopsTimeout, logger, resolver, node))
	}()

	daemon, err := newDaemon(files, host, credentials, node, parts, logger)
	if err != nil {
		return err
	}
	// serving first: if a port is taken, the agent ends before it started
	// any container
	servers, serveErrors, err := daemon.serve()
	if err != nil {
		return err
	}
	daemon.startLoops(loopsCtx, &loops, caughtUp)
	logger.Info("serving", "host", host.Name, "api", host.apiAddresses(), "raft", host.raftAddress(),
		"metrics", metricsAddress(host))
	if err := notify("READY=1"); err != nil {
		logger.Warn("systemd", "error", err)
	}

	err = watch(ctx, node, serveErrors, logger)
	daemon.server.EndStreams() // Shutdown would wait for them
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
// catchUpTimeout, says so, and returns caughtUp false.
func openNode(ctx context.Context, files Files, host Host, credentials credentials, logger *slog.Logger) (
	node *raftnode.Node, caughtUp bool, err error,
) {
	node, err = raftnode.Open(raftnode.Config{
		Dir: files.raftDir(), ID: host.ID, Bind: host.raftAddress(), Advertise: host.raftAddress(),
		Certificate: credentials.certificate, CA: credentials.ca, Logger: logger,
	})
	if err != nil {
		return nil, false, err
	}
	catchUp, cancel := context.WithTimeout(ctx, catchUpTimeout)
	defer cancel()
	if err := node.CatchUp(catchUp); err != nil {
		logger.Warn("serving before the state caught up with the Raft log", "error", err)
		return node, false, nil
	}
	return node, true, nil
}

// daemon is the running agent: its parts that run in loops or answer the
// API, and what they share.
type daemon struct {
	host        Host
	credentials credentials
	node        *raftnode.Node
	parts       HostParts
	logger      *slog.Logger
	// propose is how every part commits (serverProposer)
	propose    controller.Propose
	runner     *stackRunner
	traefik    *traefikRunner
	reporter   *hostReporter
	supervisor *supervisor.Supervisor
	conditions *controller.Conditions
	alerts     *controller.Alerts
	server     *api.Server
}

// newDaemon makes the agent's parts; it starts nothing. Who calls whom:
//
//	supervisor ──wakes (passSignal)──▶ stack runner, Traefik
//	stack runner, Traefik ──call Pause on──▶ supervisor
//	stack runner, report, alerts, leader's loops ──commit through (serverProposer)──▶ API server
//	API server ──answers for──▶ stack runner (containers), alerts
//
// The signals are made first, so the supervisor needs no runner; the
// proposer gets the server last, as the server needs the stack runner and
// the alerts. No loop runs before newDaemon returns.
func newDaemon(files Files, host Host, credentials credentials, node *raftnode.Node, parts HostParts,
	logger *slog.Logger,
) (*daemon, error) {
	stacksSignal, traefikSignal := newPassSignal(), newPassSignal()
	dockerRestarted := func() {
		stacksSignal.dockerRestarted()
		traefikSignal.dockerRestarted()
	}
	watcher := supervisor.New(parts.Docker, restartRules{node, host.ID}.of, supervisor.DefaultBackoff,
		dockerRestarted, traefikSignal.now, logger)
	proposer := &serverProposer{}

	runner, err := newStackRunner(files, host, credentials, node, parts, watcher, stacksSignal, proposer.propose, logger)
	if err != nil {
		return nil, err
	}
	traefik := newTraefikRunner(files, host, credentials, node, parts, watcher, traefikSignal, logger)
	reporter := newHostReporter(host, node, parts, proposer.propose, logger)
	conditions := controller.NewConditions()
	alerts := controller.NewAlerts(node, proposer.propose, credentials.dataKey.Open, conditions, logger)
	proposer.server = newAPIServer(host, credentials, node, runner, alerts, logger)

	return &daemon{
		host:        host,
		credentials: credentials,
		node:        node,
		parts:       parts,
		logger:      logger,
		propose:     proposer.propose,
		runner:      runner,
		traefik:     traefik,
		reporter:    reporter,
		supervisor:  watcher,
		conditions:  conditions,
		alerts:      alerts,
		server:      proposer.server,
	}, nil
}

// serverProposer commits through the API server's Propose. newDaemon sets
// the server last: the server answers for parts that commit.
type serverProposer struct {
	server *api.Server
}

func (p *serverProposer) propose(ctx context.Context, cmd state.Command) (uint64, error) {
	return p.server.Propose(ctx, cmd)
}

// newStackRunner makes the stack runner, with the key of its secrets'
// labels derived from the data key.
func newStackRunner(files Files, host Host, credentials credentials, node *raftnode.Node, parts HostParts,
	watcher *supervisor.Supervisor, signal *passSignal, propose controller.Propose, logger *slog.Logger,
) (*stackRunner, error) {
	digestKey, err := credentials.dataKey.Derive(secretLabelPurpose)
	if err != nil {
		return nil, fmt.Errorf("derive the key of the secrets' labels: %w", err)
	}
	return &stackRunner{
		files:       files,
		host:        host,
		cluster:     node,
		docker:      parts.Docker,
		storage:     parts.Storage,
		open:        credentials.dataKey.Open,
		digestKey:   digestKey,
		logger:      logger,
		supervisor:  watcher,
		signal:      signal,
		propose:     propose,
		applied:     map[string]broughtUp{},
		logins:      map[string][32]byte{},
		unexplained: map[string]bool{},
	}, nil
}

// newTraefikRunner makes Traefik's runner.
func newTraefikRunner(files Files, host Host, credentials credentials, node *raftnode.Node, parts HostParts,
	watcher *supervisor.Supervisor, signal *passSignal, logger *slog.Logger,
) *traefikRunner {
	return &traefikRunner{
		files:      files,
		host:       host,
		cluster:    node,
		docker:     parts.Docker,
		open:       credentials.dataKey.Open,
		logger:     logger,
		supervisor: watcher,
		signal:     signal,
		unopened:   map[string]bool{},
	}
}

// newHostReporter makes the report of this host, of its root file system.
func newHostReporter(host Host, node *raftnode.Node, parts HostParts, propose controller.Propose,
	logger *slog.Logger,
) *hostReporter {
	return &hostReporter{
		host:    host,
		version: buildinfo.Version,
		cluster: node,
		docker:  parts.Docker,
		storage: parts.Storage,
		propose: propose,
		logger:  logger,
		root:    "/",
	}
}

// newAPIServer makes the API server, which answers for the stack runner
// (a stack's containers) and the alerts.
func newAPIServer(host Host, credentials credentials, node *raftnode.Node, runner *stackRunner,
	alerts *controller.Alerts, logger *slog.Logger,
) *api.Server {
	return api.NewServer(api.ServerConfig{
		Cluster: node, CA: credentials.ca, Certificate: credentials.certificate,
		APIAddress: func(id state.HostID) string {
			// every host has the same API port; tests on 127.0.0.1 have one host
			return net.JoinHostPort(network.NodeAddress(id).String(), strconv.Itoa(host.APIPort))
		},
		Keys: credentials.dataKey, Version: buildinfo.Version, Logger: logger,
		HostID: host.ID, Containers: runner, Alerts: alertsAPI{alerts},
	})
}

// startLoops starts every loop of the agent; each ends with ctx. caughtUp
// is openNode's: whether the state caught up at the start.
func (d *daemon) startLoops(ctx context.Context, loops *sync.WaitGroup, caughtUp bool) {
	node, propose, logger := d.node, d.propose, d.logger

	// this host's loops that start no container
	loops.Go(func() { reconcileNetwork(ctx, node, d.parts.Network, logger) })
	loops.Go(func() { d.supervisor.Run(ctx, compose.LabelStack) })
	loops.Go(func() { d.runner.trim(ctx) })

	// the leader's loops: each does nothing while this host does not lead
	loops.Go(func() { controller.RunPlacement(ctx, node, propose, d.conditions, logger) })
	loops.Go(func() {
		controller.RunFloatingIP(ctx, node, propose, d.credentials.dataKey.Open, d.parts.Hoster, d.conditions, logger)
	})
	loops.Go(func() { controller.RunACME(ctx, node, propose, d.credentials.dataKey, logger) })
	loops.Go(func() { d.alerts.Run(ctx) })
	loops.Go(func() { d.alerts.RunHeartbeat(ctx) })

	// the loops that wait for the state
	loops.Go(func() { d.runOnceCaughtUp(ctx, loops, caughtUp) })
}

// runOnceCaughtUp runs the stacks, Traefik and the report once the state
// caught up. Until then the state may still place a stack on this host
// that has already moved to another one, so containers must not start yet.
// The report waits too, as it compares the host with the state.
func (d *daemon) runOnceCaughtUp(ctx context.Context, loops *sync.WaitGroup, caughtUp bool) {
	if !caughtUp && !waitForState(ctx, d.node, d.logger) {
		return
	}
	// this function runs as one of the loops, so loops.Wait cannot have
	// returned yet: adding to it from here is allowed
	loops.Go(func() { d.reporter.run(ctx) })
	loops.Go(func() { d.traefik.run(ctx) })
	d.runner.run(ctx)
}

// errLoopsLeft ends the agent when a loop did not end in time.
var errLoopsLeft = errors.New("a loop did not end in time; the process ends with it")

// stopLoops ends the agent's loops and waits for them, at most timeout.
// Every host tool a pass runs has a timeout, so a loop normally ends soon;
// but a tool hung on a suspended DRBD volume never ends, and must not hold
// up the stop. Once all loops ended, stopLoops closes what they use (the
// resolver, the Raft member). If a loop is left behind, it may still use
// them, so stopLoops closes nothing and returns errLoopsLeft: the process
// ends right after, which closes them anyway, and systemd sees the
// failure. Raft's log survives that; it is on disk.
func stopLoops(stop context.CancelFunc, loops *sync.WaitGroup, timeout time.Duration, logger *slog.Logger,
	used ...io.Closer,
) error {
	stop()
	ended := make(chan struct{})
	go func() {
		loops.Wait()
		close(ended)
	}()
	select {
	case <-ended:
	case <-time.After(timeout):
		logger.Error("stopping before every loop ended", "after", timeout)
		return errLoopsLeft
	}
	for _, closer := range used {
		_ = closer.Close() // stopping: nothing to keep
	}
	return nil
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
