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
	"github.com/webcodingcat/clowder/internal/atomicfile"
	"github.com/webcodingcat/clowder/internal/buildinfo"
	"github.com/webcodingcat/clowder/internal/compose"
	"github.com/webcodingcat/clowder/internal/controller"
	"github.com/webcodingcat/clowder/internal/docker"
	"github.com/webcodingcat/clowder/internal/hetzner"
	"github.com/webcodingcat/clowder/internal/hostrole"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/network"
	"github.com/webcodingcat/clowder/internal/raftnode"
	"github.com/webcodingcat/clowder/internal/state"
	"github.com/webcodingcat/clowder/internal/storage"
	"github.com/webcodingcat/clowder/internal/supervisor"
)

const (
	shutdownTimeout = 5 * time.Second
	// loopsTimeout is well within systemd's stop timeout.
	loopsTimeout = 30 * time.Second
	// catchUpTimeout is well within systemd's start timeout.
	catchUpTimeout = 15 * time.Second
)

// HostParts are the parts of the host that the agent changes: its network,
// storage and Docker. Tests use fakes.
type HostParts struct {
	Network HostNetwork
	Storage Storage
	Docker  Docker
}

// Docker is what the agent and its runners need from Docker. docker.Client
// is the real implementation.
type Docker interface {
	supervisor.Docker
	stacks.Docker
	stacks.Puller
	Inspect(ctx context.Context, id string) (docker.Details, error)
	Ping(ctx context.Context) error
}

// Storage is what the agent and the volume reconciler need from the
// storage driver. storage.Driver is the real implementation.
type Storage interface {
	volumes.Storage
	PoolUsage(ctx context.Context, pool string) (sizeBytes int64, dataPercent, metadataPercent float64, err error)
	ForgetPeer(ctx context.Context, peer int) error
}

// Run runs the agent until ctx ends, in the order docs/developing/host.md
// describes (clowderd run). It tells systemd when it is ready to serve, and
// it keeps feeding systemd's watchdog while the state answers.
func Run(ctx context.Context, files Files, parts HostParts, logger *slog.Logger) (err error) {
	if files.SecretsDir == "" {
		return errors.New("run needs a directory for the stacks' secrets")
	}
	// SecretsDir may be left over from an earlier run or changed by hand, so
	// its owner and mode are set at each start. The runners do the same for
	// the directories inside it: Traefik's and each stack's.
	if err := atomicfile.EnsureDir(files.SecretsDir, 0o700); err != nil {
		return err
	}
	host, err := files.readHost()
	if err != nil {
		return err
	}
	// a join that wrote host.json but could not remove join.json leaves a
	// second copy of the keys in join.json
	if err := os.Remove(files.joinFile()); err != nil && !errors.Is(err, os.ErrNotExist) {
		logger.Warn("remove what the join left", "file", files.joinFile(), "error", err)
	}
	credentials, err := files.readCredentials(host.Roles)
	if err != nil {
		return err
	}
	// Raft is not running yet, so the state comes from its log on disk
	onDisk, err := raftnode.ReadState(files.raftDir())
	if err != nil {
		return fmt.Errorf("read the state before the start: %w", err)
	}
	if err := host.checkRoles(credentials.certificate.Leaf, onDisk); err != nil {
		return err
	}
	if err := checkRetired(ctx, onDisk, host, credentials, logger); err != nil {
		return err
	}
	if err := parts.Network.SetUpInterface(ctx, host.NodeAddress); err != nil {
		return err
	}
	if err := restoreMesh(ctx, files, host.ID, parts.Network); err != nil {
		// the state brings the mesh too, if a leader can be reached without it
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
	// and the Raft member. Before any loop has started, it only closes them.
	defer func() {
		err = errors.Join(err, stopLoops(cancelLoops, &loops, loopsTimeout, logger, resolver, node))
	}()

	daemon := newDaemon(shared{
		files: files, host: host, credentials: credentials, node: node, parts: parts, logger: logger,
	})
	// serve first: if a port is taken, the agent ends before it has
	// started any container
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

// openNode waits a while for the state to catch up with the log, so that
// the agent answers from the state in the log, not from an empty one. A
// host without quorum starts anyway after catchUpTimeout.
func openNode(ctx context.Context, files Files, host Host, credentials credentials, logger *slog.Logger) (
	*raftnode.Node, error,
) {
	node, err := raftnode.Open(raftnode.Config{
		Dir: files.raftDir(), ID: host.ID, Bind: host.raftAddress(), Advertise: host.raftAddress(),
		Certificate: credentials.certificate, CA: credentials.ca, Logger: logger,
	})
	if errors.Is(err, state.ErrUnreadable) {
		// Open restores the last snapshot and could not read it
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
	// may still reach such an entry later, while it applies the leader's
	// log. Then watch ends the agent.
	if err := node.HaltError(); err != nil {
		_ = node.Close() // the log is on disk; nothing is lost
		return nil, haltError(err)
	}
	return node, nil
}

// shared holds what every part of the agent is built from.
type shared struct {
	files       Files
	host        Host
	credentials credentials
	node        *raftnode.Node
	parts       HostParts
	logger      *slog.Logger
}

type daemon struct {
	shared
	// propose commits a command for every part, through the API server's
	// leader forwarding (lateServer)
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
	// caughtUp is closed once the state has caught up after the start. The
	// API holds users' calls until then (api.ServerConfig.CaughtUp).
	caughtUp chan struct{}
}

// newDaemon creates the agent's parts but starts nothing. Who calls whom:
//
//	supervisor ──wakes (loop.Signal)──▶ stack runner, Traefik
//	volume reconciler ──wakes (loop.Signal)──▶ stack runner
//	stack runner ──wakes (loop.Signal) once a leaver let go──▶ volume reconciler
//	stack runner ──asks (Mounted)──▶ volume reconciler
//	stack runner, Traefik ──call Pause on──▶ supervisor
//	stack runner, volume reconciler, report, alerts, leader's loops ──commit through (lateServer)──▶ API server
//	stack runner, Traefik ──open secrets and keys through (lateServer)──▶ API server
//	API server ──answers for──▶ stack runner (containers), volume reconciler (progress), alerts
//	move steps ──ask──▶ prefetcher, stack runner's files, volume reconciler
//
// The signals are created first, so the supervisor does not need the
// runners. lateServer gets the server last, because the server needs the
// stack runner and the alerts. No loop runs before newDaemon returns.
func newDaemon(base shared) *daemon {
	stacksSignal, traefikSignal, volumesSignal := loop.NewSignal(), loop.NewSignal(), loop.NewSignal()
	dockerRestarted := func() {
		stacksSignal.DockerRestarted()
		traefikSignal.DockerRestarted()
	}
	watcher := supervisor.New(base.parts.Docker, restartRules{base.node, base.host.ID}.of, supervisor.DefaultBackoff,
		dockerRestarted, traefikSignal.Now, base.logger)
	late := &lateServer{}

	reconciler := newVolumeReconciler(base, volumesSignal, stacksSignal, late.propose)
	openSecret := newSecretOpener(late)
	// the runner and the prefetcher write the namespaces' Docker configs
	registries := stacks.NewRegistryConfigs(base.files.stackFiles(), openSecret)
	runner := newStackRunner(base, watcher, stacksSignal, volumesSignal, registries, reconciler, openSecret, late.propose)
	conditions := controller.NewConditions()
	alerts := controller.NewAlerts(base.node, late.propose, base.credentials.decrypt, conditions, base.logger)
	joins := &joiner{cluster: base.node, propose: late.propose, dataKey: base.credentials.dataKey}
	heard := controller.NewHeard()
	caughtUp := make(chan struct{})
	late.server = newAPIServer(base, runner, reconciler, alerts, heard, joins, caughtUp)

	return &daemon{
		shared:     base,
		propose:    late.propose,
		volumes:    reconciler,
		runner:     runner,
		prefetcher: stacks.NewPrefetcher(base.host.ID, base.node, base.parts.Docker, registries, base.logger),
		traefik:    newTraefikRunner(base, watcher, traefikSignal, newCertificateKeys(late)),
		reporter:   newHostReporter(base, late.propose, reconciler.Seen),
		supervisor: watcher,
		conditions: conditions,
		heard:      heard,
		alerts:     alerts,
		server:     late.server,
		caughtUp:   caughtUp,
	}
}

// lateServer reaches the API server, which newDaemon sets last, because
// the server needs parts that already commit and open secrets through it.
// No part calls it before newDaemon returns.
type lateServer struct {
	server *api.Server
}

func (l *lateServer) propose(ctx context.Context, cmd state.Command) (uint64, error) {
	return l.server.Propose(ctx, cmd)
}

// newSecretOpener opens the secrets of the stacks that this host runs,
// through what the control plane gives it.
func newSecretOpener(late *lateServer) stacks.OpenSecret {
	secrets := &controlPlaneCache[api.HostSecret]{
		what: "secret",
		fetch: func(ctx context.Context) (map[string]api.HostSecret, error) {
			return late.server.SecretsOfThisHost(ctx)
		},
		version: func(secret api.HostSecret) string { return secret.Digest },
	}
	return func(ctx context.Context, secret *state.Secret) (map[string][]byte, error) {
		opened, err := secrets.get(ctx, names.ID(secret.Namespace, secret.Name), secret.Spec.Digest)
		return opened.Data, err
	}
}

// cachedCertificateKeys opens the certificates' keys while this host holds
// the Floating IP, through what the control plane gives it.
type cachedCertificateKeys struct {
	cache *controlPlaneCache[api.HostCertificate]
}

func newCertificateKeys(late *lateServer) cachedCertificateKeys {
	return cachedCertificateKeys{cache: &controlPlaneCache[api.HostCertificate]{
		what: "certificate",
		fetch: func(ctx context.Context) (map[string]api.HostCertificate, error) {
			return late.server.CertificatesOfThisHost(ctx)
		},
		version: func(certificate api.HostCertificate) string { return chainDigest(certificate.Chain) },
	}}
}

func (k cachedCertificateKeys) open(ctx context.Context, certificate state.Certificate) ([]byte, error) {
	opened, err := k.cache.get(ctx, certificate.Domain, chainDigest(certificate.Chain))
	return opened.Key, err
}

func (k cachedCertificateKeys) forget() { k.cache.forget() }

func newVolumeReconciler(base shared, signal, stacksSignal *loop.Signal, propose controller.Propose,
) *volumes.Reconciler {
	return volumes.New(volumes.Parts{
		Files:       base.files.volumeFiles(),
		Host:        base.host.ID,
		Cluster:     base.node,
		Storage:     base.parts.Storage,
		Held:        base.files.stackFiles().HeldLVs,
		Signal:      signal,
		StacksReady: stacksSignal.Now,
		Propose:     propose,
		Logger:      base.logger,
	})
}

func newStackRunner(base shared, watcher loop.Pauser, signal, volumesSignal *loop.Signal,
	registries *stacks.RegistryConfigs, volumes stacks.Volumes, openSecret stacks.OpenSecret, propose controller.Propose,
) *stacks.Runner {
	return stacks.New(stacks.Parts{
		Files:           base.files.stackFiles(),
		Host:            base.host.ID,
		NodeAddress:     base.host.NodeAddress,
		Cluster:         base.node,
		Docker:          base.parts.Docker,
		Volumes:         volumes,
		OpenSecret:      openSecret,
		Supervisor:      watcher,
		Signal:          signal,
		Registries:      registries,
		VolumesReleased: volumesSignal.Now,
		Propose:         propose,
		Logger:          base.logger,
	})
}

func newTraefikRunner(base shared, watcher loop.Pauser, signal *loop.Signal, keys certificateKeys) *traefikRunner {
	return &traefikRunner{
		files:      base.files,
		host:       base.host,
		cluster:    base.node,
		docker:     base.parts.Docker,
		keys:       keys,
		logger:     base.logger,
		supervisor: watcher,
		signal:     signal,
		unopened:   map[string]bool{},
	}
}

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

func newAPIServer(base shared, runner *stacks.Runner, volumes api.Volumes, alerts *controller.Alerts,
	heard *controller.Heard, joins api.Joins, caughtUp <-chan struct{},
) *api.Server {
	// a nil *datakey.Key in the interface would not compare as nil
	var keys api.Keys
	if base.credentials.dataKey != nil {
		keys = base.credentials.dataKey
	}
	return api.NewServer(api.ServerConfig{
		Cluster: base.node, CA: base.credentials.ca, Certificate: base.credentials.certificate, Roles: base.host.Roles,
		APIAddress: func(id state.HostID) string {
			// every host has the same API port; tests on 127.0.0.1 have one host
			return net.JoinHostPort(network.NodeAddress(id).String(), strconv.Itoa(base.host.APIPort))
		},
		Keys: keys, Version: buildinfo.Version, Logger: base.logger,
		HostID: base.host.ID, Containers: runner, Volumes: volumes, Alerts: alertsAPI{alerts}, Beats: heard,
		Joins: joins, CaughtUp: caughtUp,
		HetznerServers: func(ctx context.Context, hetznerAPI, token string) ([]int64, error) {
			ctx, cancel := context.WithTimeout(ctx, hetznerCheckTimeout)
			defer cancel()
			return hetzner.New(hetznerAPI, token).ServerIDs(ctx)
		},
	})
}

// hetznerCheckTimeout limits the single request with which the dry run
// checks a new Hetzner token, because the user waits for the answer.
const hetznerCheckTimeout = 20 * time.Second

func (d *daemon) startLoops(ctx context.Context, loops *sync.WaitGroup) {
	node, logger := d.node, d.logger

	// this host's loops that start no container
	loops.Go(func() { reconcileNetwork(ctx, node, d.files, d.host.ID, d.parts.Network, logger) })
	loops.Go(func() { d.supervisor.Run(ctx, compose.LabelStack) })
	loops.Go(func() { sendBeats(ctx, node, d.host.ID, d.server.Beat, logger) })
	if d.host.Roles.Has(hostrole.ControlPlane) {
		d.startControlPlaneLoops(ctx, loops)
	}

	// the loops that wait for the state
	loops.Go(func() { d.runOnceCaughtUp(ctx, loops) })
}

// startControlPlaneLoops starts the loops that need the data key: the
// leader's, and the watch that alerts while there is no leader. A host
// without the control-plane role never votes, so it never leads, and it
// holds neither the token nor the alert channels' secrets. A worker cut off
// from every control-plane host alerts nobody; the voters see it fail.
func (d *daemon) startControlPlaneLoops(ctx context.Context, loops *sync.WaitGroup) {
	node, propose, logger := d.node, d.propose, d.logger
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
		controller.RunFloatingIP(ctx, node, propose, d.credentials.decrypt, d.conditions, logger)
	})
	loops.Go(func() { controller.RunACME(ctx, node, propose, d.credentials.dataKey, logger) })
	loops.Go(func() {
		controller.RunFencing(ctx, controller.FenceParts{
			Cluster: node, Propose: propose, Open: d.credentials.decrypt,
			Heard: d.heard, Ask: d.server.AskSilence, Self: d.host.ID, Conditions: d.conditions, Logger: logger,
		})
	})
	loops.Go(func() { d.alerts.Run(ctx) })
	loops.Go(func() { d.alerts.RunHeartbeat(ctx) })
	// leading or not: it alerts while there is no leader
	loops.Go(func() {
		knowsLeader := func() bool {
			_, ok := d.node.Leader()
			return ok
		}
		controller.NewQuorumWatch(d.alerts, d.host.Name, knowsLeader).Run(ctx)
	})
}

// runOnceCaughtUp waits because until the state has caught up, it may still
// place a stack here that has already moved to another host: no container
// may start yet, and no volume of that stack may become primary here. The
// report waits too, because it compares the host with the state.
func (d *daemon) runOnceCaughtUp(ctx context.Context, loops *sync.WaitGroup) {
	if !waitForState(ctx, d.node, d.server, d.logger) {
		return
	}
	close(d.caughtUp)
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

var errLoopsLeft = errors.New("a loop did not end in time; the process ends with it")

// stopLoops waits for at most timeout, because a tool that hangs on a
// suspended DRBD volume never ends. A loop left behind may still use what
// used holds, so then stopLoops closes nothing; the process exit closes it,
// and Raft's log is on disk.
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

// waitForState reports whether the state caught up with the leader's
// commit index before ctx ended.
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
		// a refusal returns immediately; pause so the loop does not spin
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
	return false
}

// haltError keeps the error a state.ErrUnreadable, which ends the process
// with its own exit status.
func haltError(err error) error {
	return fmt.Errorf("stopped at what this agent cannot read in the Raft log: written by a newer "+
		"version (install the version the leader runs), or damaged: %w", err)
}

// watch feeds systemd's watchdog only while the state answers, so that
// systemd restarts a deadlocked agent.
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
