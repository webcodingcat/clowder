package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/webcodingcat/clowder/internal/agent/loop"
	"github.com/webcodingcat/clowder/internal/agent/stacks"
	"github.com/webcodingcat/clowder/internal/agent/volumes"
	"github.com/webcodingcat/clowder/internal/api"
	"github.com/webcodingcat/clowder/internal/buildinfo"
	"github.com/webcodingcat/clowder/internal/compose"
	"github.com/webcodingcat/clowder/internal/controller"
	"github.com/webcodingcat/clowder/internal/docker"
	"github.com/webcodingcat/clowder/internal/network"
	"github.com/webcodingcat/clowder/internal/raftnode"
	"github.com/webcodingcat/clowder/internal/state"
	"github.com/webcodingcat/clowder/internal/storage"
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

// Docker is what the agent and its runners need from Docker; docker.Client
// is the real one.
type Docker interface {
	supervisor.Docker
	stacks.Docker
	stacks.Puller
	Inspect(ctx context.Context, id string) (docker.Details, error)
	Ping(ctx context.Context) error
}

// Storage is what the agent and the volume reconciler need from the
// storage driver; storage.Driver is the real one.
type Storage interface {
	volumes.Storage
	PoolUsage(ctx context.Context, pool string) (sizeBytes int64, dataPercent, metadataPercent float64, err error)
	ForgetPeer(ctx context.Context, peer int) error
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
	// a join that wrote host.json and could not remove join.json leaves the
	// data key a second time there
	if err := os.Remove(files.joinFile()); err != nil && !errors.Is(err, os.ErrNotExist) {
		logger.Warn("remove what the join left", "file", files.joinFile(), "error", err)
	}
	credentials, err := files.readCredentials()
	if err != nil {
		return err
	}
	if err := parts.Network.SetUpInterface(ctx, host.NodeAddress); err != nil {
		return err
	}
	if err := restoreMesh(ctx, files, host.ID, parts.Network); err != nil {
		// the state brings the mesh too, if a leader is in reach without it
		logger.Error("start without the mesh of the last run", "error", err)
	}
	node, err := openNode(ctx, files, host, credentials, logger)
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

	daemon, err := newDaemon(shared{
		files: files, host: host, credentials: credentials, node: node, parts: parts, logger: logger,
	})
	if err != nil {
		return err
	}
	// serving first: if a port is taken, the agent ends before it started
	// any container
	servers, serveErrors, err := daemon.serve()
	if err != nil {
		return err
	}
	daemon.startLoops(loopsCtx, &loops)
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
// catchUpTimeout and says so. Before it starts stacks, the agent catches up
// with the leader's commit index too (waitForState).
func openNode(ctx context.Context, files Files, host Host, credentials credentials, logger *slog.Logger) (
	*raftnode.Node, error,
) {
	node, err := raftnode.Open(raftnode.Config{
		Dir: files.raftDir(), ID: host.ID, Bind: host.raftAddress(), Advertise: host.raftAddress(),
		Certificate: credentials.certificate, CA: credentials.ca, Logger: logger,
	})
	if errors.Is(err, state.ErrUnreadable) {
		// Open restores the last snapshot, and could not read it
		return nil, haltError(err)
	}
	if err != nil {
		return nil, err
	}
	catchUp, cancel := context.WithTimeout(ctx, catchUpTimeout)
	defer cancel()
	if err := node.CatchUp(catchUp); err != nil {
		logger.Warn("serving before the state caught up with the Raft log", "error", err)
	}
	// A halt this early ends the agent before its loops start. A follower
	// may still meet the entry later, as it applies the leader's log; then
	// watch ends the agent.
	if err := node.HaltError(); err != nil {
		_ = node.Close() // the log is on disk; nothing is lost
		return nil, haltError(err)
	}
	return node, nil
}

// shared is what every part of the agent is made with.
type shared struct {
	files       Files
	host        Host
	credentials credentials
	node        *raftnode.Node
	parts       HostParts
	logger      *slog.Logger
}

// daemon is the running agent: its parts that run in loops or answer the
// API, and what they share.
type daemon struct {
	shared
	// propose is how every part commits (serverProposer)
	propose    controller.Propose
	volumes    *volumes.Reconciler
	runner     *stacks.Runner
	prefetcher *stacks.Prefetcher
	traefik    *traefikRunner
	reporter   *hostReporter
	supervisor *supervisor.Supervisor
	conditions *controller.Conditions
	heard      *controller.Heard
	alerts     *controller.Alerts
	server     *api.Server
}

// newDaemon makes the agent's parts; it starts nothing. Who calls whom:
//
//	supervisor ──wakes (loop.Signal)──▶ stack runner, Traefik
//	volume reconciler ──wakes (loop.Signal)──▶ stack runner
//	stack runner ──asks (Mounted)──▶ volume reconciler
//	stack runner, Traefik ──call Pause on──▶ supervisor
//	stack runner, volume reconciler, report, alerts, leader's loops ──commit through (serverProposer)──▶ API server
//	API server ──answers for──▶ stack runner (containers), volume reconciler (progress), alerts
//	move steps ──ask──▶ prefetcher, stack runner's files, volume reconciler
//
// The signals are made first, so the supervisor needs no runner; the
// proposer gets the server last, as the server needs the stack runner and
// the alerts. No loop runs before newDaemon returns.
func newDaemon(base shared) (*daemon, error) {
	stacksSignal, traefikSignal := loop.NewSignal(), loop.NewSignal()
	dockerRestarted := func() {
		stacksSignal.DockerRestarted()
		traefikSignal.DockerRestarted()
	}
	watcher := supervisor.New(base.parts.Docker, restartRules{base.node, base.host.ID}.of, supervisor.DefaultBackoff,
		dockerRestarted, traefikSignal.Now, base.logger)
	proposer := &serverProposer{}

	reconciler := newVolumeReconciler(base, stacksSignal, proposer.propose)
	runner, err := newStackRunner(base, watcher, stacksSignal, reconciler, proposer.propose)
	if err != nil {
		return nil, err
	}
	conditions := controller.NewConditions()
	alerts := controller.NewAlerts(base.node, proposer.propose, base.credentials.dataKey.Open, conditions, base.logger)
	joins := &joiner{cluster: base.node, propose: proposer.propose, dataKey: base.credentials.dataKey, connect: base.parts.Hoster}
	heard := controller.NewHeard()
	proposer.server = newAPIServer(base, runner, reconciler, alerts, heard, joins)

	return &daemon{
		shared:     base,
		propose:    proposer.propose,
		volumes:    reconciler,
		runner:     runner,
		prefetcher: stacks.NewPrefetcher(base.host.ID, base.node, base.parts.Docker, base.logger),
		traefik:    newTraefikRunner(base, watcher, traefikSignal),
		reporter:   newHostReporter(base, proposer.propose, reconciler.Seen),
		supervisor: watcher,
		conditions: conditions,
		heard:      heard,
		alerts:     alerts,
		server:     proposer.server,
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

// newVolumeReconciler makes the volume reconciler, which leaves alone the
// copies the stacks' containers may hold and wakes the stack runner when a
// copy is mounted.
func newVolumeReconciler(base shared, stacksSignal *loop.Signal, propose controller.Propose) *volumes.Reconciler {
	return volumes.New(volumes.Parts{
		Files:       base.files.volumeFiles(),
		Host:        base.host.ID,
		Cluster:     base.node,
		Storage:     base.parts.Storage,
		Held:        base.files.stackFiles().HeldLVs,
		Signal:      loop.NewSignal(),
		StacksReady: stacksSignal.Now,
		Propose:     propose,
		Logger:      base.logger,
	})
}

// newStackRunner makes the stack runner, with the key of its secrets'
// labels derived from the data key.
func newStackRunner(base shared, watcher loop.Pauser, signal *loop.Signal, volumes stacks.Volumes,
	propose controller.Propose,
) (*stacks.Runner, error) {
	digestKey, err := base.credentials.dataKey.Derive(stacks.SecretLabelPurpose)
	if err != nil {
		return nil, fmt.Errorf("derive the key of the secrets' labels: %w", err)
	}
	return stacks.New(stacks.Parts{
		Files:       base.files.stackFiles(),
		Host:        base.host.ID,
		NodeAddress: base.host.NodeAddress,
		Cluster:     base.node,
		Docker:      base.parts.Docker,
		Volumes:     volumes,
		Open:        base.credentials.dataKey.Open,
		DigestKey:   digestKey,
		Supervisor:  watcher,
		Signal:      signal,
		Propose:     propose,
		Logger:      base.logger,
	}), nil
}

// newTraefikRunner makes Traefik's runner.
func newTraefikRunner(base shared, watcher loop.Pauser, signal *loop.Signal) *traefikRunner {
	return &traefikRunner{
		files:      base.files,
		host:       base.host,
		cluster:    base.node,
		docker:     base.parts.Docker,
		open:       base.credentials.dataKey.Open,
		logger:     base.logger,
		supervisor: watcher,
		signal:     signal,
		unopened:   map[string]bool{},
	}
}

// newHostReporter makes the report of this host, of its root file system.
func newHostReporter(base shared, propose controller.Propose, seen func() map[string]storage.Observed) *hostReporter {
	return &hostReporter{
		seen:    seen,
		host:    base.host,
		version: buildinfo.Version,
		cluster: base.node,
		docker:  base.parts.Docker,
		storage: base.parts.Storage,
		propose: propose,
		logger:  base.logger,
		root:    "/",
	}
}

// newAPIServer makes the API server, which answers for the stack runner
// (a stack's containers), the alerts, the hosts' beats and the joins.
func newAPIServer(base shared, runner *stacks.Runner, volumes api.Volumes, alerts *controller.Alerts,
	heard *controller.Heard, joins api.Joins,
) *api.Server {
	return api.NewServer(api.ServerConfig{
		Cluster: base.node, CA: base.credentials.ca, Certificate: base.credentials.certificate,
		APIAddress: func(id state.HostID) string {
			// every host has the same API port; tests on 127.0.0.1 have one host
			return net.JoinHostPort(network.NodeAddress(id).String(), strconv.Itoa(base.host.APIPort))
		},
		Keys: base.credentials.dataKey, Version: buildinfo.Version, Logger: base.logger,
		HostID: base.host.ID, Containers: runner, Volumes: volumes, Alerts: alertsAPI{alerts}, Beats: heard,
		Joins: joins,
	})
}

// startLoops starts every loop of the agent; each ends with ctx.
func (d *daemon) startLoops(ctx context.Context, loops *sync.WaitGroup) {
	node, propose, logger := d.node, d.propose, d.logger

	// this host's loops that start no container
	loops.Go(func() { reconcileNetwork(ctx, node, d.files, d.host.ID, d.parts.Network, logger) })
	loops.Go(func() { d.supervisor.Run(ctx, compose.LabelStack) })
	loops.Go(func() { sendBeats(ctx, node, d.host.ID, d.server.Beat, logger) })

	// the leader's loops: each does nothing while this host does not lead
	loops.Go(func() {
		// every host has the same Raft port
		raftAddress := func(id state.HostID) string {
			return net.JoinHostPort(network.NodeAddress(id).String(), strconv.Itoa(d.host.RaftPort))
		}
		controller.RunMembers(ctx, node, node, propose, raftAddress, d.host.ID, logger)
	})
	loops.Go(func() { controller.RunPlacement(ctx, node, propose, d.conditions, logger) })
	loops.Go(func() { controller.RunMoves(ctx, node, propose, d.server.Progress, logger) })
	loops.Go(func() {
		controller.RunFloatingIP(ctx, node, propose, d.credentials.dataKey.Open, d.parts.Hoster, d.conditions, logger)
	})
	loops.Go(func() { controller.RunACME(ctx, node, propose, d.credentials.dataKey, logger) })
	loops.Go(func() {
		controller.RunFencing(ctx, controller.FenceParts{
			Cluster: node, Propose: propose, Open: d.credentials.dataKey.Open, Connect: d.parts.Hoster,
			Heard: d.heard, Ask: d.server.AskSilence, Self: d.host.ID, Conditions: d.conditions, Logger: logger,
		})
	})
	loops.Go(func() { d.alerts.Run(ctx) })
	loops.Go(func() { d.alerts.RunHeartbeat(ctx) })

	// the loops that wait for the state
	loops.Go(func() { d.runOnceCaughtUp(ctx, loops) })
}

// runOnceCaughtUp runs the volumes, the stacks, Traefik and the report once
// the state caught up. Until then the state may still place a stack on
// this host that has already moved to another one, so containers must not
// start yet, nor its volume become primary here. The report waits too, as
// it compares the host with the state.
func (d *daemon) runOnceCaughtUp(ctx context.Context, loops *sync.WaitGroup) {
	if !waitForState(ctx, d.node, d.server, d.logger) {
		return
	}
	// this function runs as one of the loops, so loops.Wait cannot have
	// returned yet: adding to it from here is allowed
	loops.Go(func() { d.reporter.run(ctx) })
	loops.Go(func() { d.traefik.run(ctx) })
	loops.Go(func() { d.volumes.Run(ctx) })
	loops.Go(func() { d.volumes.Trim(ctx) })
	loops.Go(func() { forgetPeers(ctx, d.node, d.host.ID, d.parts.Storage, d.propose, d.logger) })
	loops.Go(func() { d.prefetcher.Run(ctx) })
	loops.Go(func() { reportRecovery(ctx, d.node, d.host.ID, d.propose, d.logger) })
	loops.Go(func() {
		moveSteps{
			cluster: d.node, self: d.host.ID, propose: d.propose, imagesReady: d.prefetcher.Ready,
			stopped: d.files.stackFiles().Stopped, released: d.volumes.Released, logger: d.logger,
		}.run(ctx)
	})
	d.runner.Run(ctx)
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
// log, as far as the leader has committed it
// (api.Server.CatchUpWithLeader); it tells whether it did.
func waitForState(ctx context.Context, node *raftnode.Node, server *api.Server, logger *slog.Logger) bool {
	for ctx.Err() == nil {
		catchUp, cancel := context.WithTimeout(ctx, catchUpTimeout)
		err := node.CatchUp(catchUp)
		if err == nil {
			err = server.CatchUpWithLeader(catchUp)
		}
		cancel()
		if err == nil {
			logger.Info("the state caught up with the leader; starting the stacks")
			return true
		}
		logger.Warn("the state has not caught up with the leader yet", "error", err)
		// a refusal returns at once; do not spin
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
	return false
}

// haltError says why the agent stops at a Raft entry or snapshot it cannot
// read, and what helps: the error stays a state.ErrUnreadable, which ends
// the process with an exit status of its own.
func haltError(err error) error {
	return fmt.Errorf("stopped at what this agent cannot read in the Raft log: written by a newer "+
		"version (install the version the leader runs), or damaged: %w", err)
}

// watch runs until ctx ends, the API fails or the Raft member halts, and
// feeds systemd's watchdog while the state answers: a deadlocked agent stops
// feeding it and systemd restarts it.
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
		case <-node.Halted():
			return haltError(node.HaltError())
		case <-tick:
			node.Read(func(*state.State) {}) // hangs if the state's lock does
			if err := notify("WATCHDOG=1"); err != nil {
				logger.Warn("systemd", "error", err)
			}
		}
	}
}
