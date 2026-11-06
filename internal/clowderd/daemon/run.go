// Package daemon is clowderd run: the agent that runs on every host, with
// its loops, its API server and its Raft member.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/webcodingcat/clowder/internal/api"
	"github.com/webcodingcat/clowder/internal/atomicfile"
	"github.com/webcodingcat/clowder/internal/buildinfo"
	"github.com/webcodingcat/clowder/internal/clowderd/daemon/loop"
	"github.com/webcodingcat/clowder/internal/clowderd/daemon/stacks"
	"github.com/webcodingcat/clowder/internal/clowderd/daemon/volumes"
	"github.com/webcodingcat/clowder/internal/clowderd/hostfiles"
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
	// catchUpPause is the pause between two tries to catch up: a leader
	// that refuses answers at once, and the agent would ask it again and
	// again without one.
	catchUpPause = time.Second
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
	DiskStates(ctx context.Context) (map[string]string, error)
	DropPeers(ctx context.Context, peers []int) error
}

// NewCommand returns clowderd run, which changes the host through parts.
func NewCommand(files hostfiles.Files, parts HostParts) *cobra.Command {
	return &cobra.Command{
		Use:   "run",
		Short: "Run the agent (clowderd.service)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGTERM, os.Interrupt)
			defer stop()
			return Run(ctx, files, parts, slog.Default())
		},
	}
}

// Run runs the agent until ctx ends, in the order docs/developing/host.md
// describes (clowderd run). It tells systemd when it is ready to serve, and
// it keeps feeding systemd's watchdog while the state answers.
func Run(ctx context.Context, files hostfiles.Files, parts HostParts, logger *slog.Logger) (err error) {
	// SecretsDir may be left over from an earlier run or changed by hand, so
	// its owner and mode are set at each start. The runners do the same for
	// the directories inside it: Traefik's and each stack's.
	if err := atomicfile.EnsureDir(files.SecretsDir, 0o700); err != nil {
		return err
	}
	host, err := files.ReadHost()
	if err != nil {
		return err
	}
	// a join that wrote host.json but could not remove join.json leaves a
	// second copy of the keys in join.json
	if err := os.Remove(files.JoinFile()); err != nil && !errors.Is(err, os.ErrNotExist) {
		logger.Warn("remove what the join left", "file", files.JoinFile(), "error", err)
	}
	credentials, err := files.ReadCredentials(host.Roles)
	if err != nil {
		return err
	}
	if err := setRaftAsideToRejoin(files, time.Now(), logger); err != nil {
		return err
	}
	// Raft is not running yet, so the state comes from its log on disk
	onDisk, err := raftnode.ReadState(files.RaftDir())
	if err != nil {
		return fmt.Errorf("read the state before the start: %w", err)
	}
	if err := host.CheckRoles(credentials.Certificate.Leaf, onDisk); err != nil {
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

	agent := newDaemon(shared{
		files: files, host: host, credentials: credentials, node: node, parts: parts, logger: logger,
	})
	// serve first: if a port is taken, the agent ends before it has
	// started any container
	servers, serveErrors, err := agent.serve()
	if err != nil {
		return err
	}
	agent.startLoops(loopsCtx, &loops)
	logger.Info("serving", "host", host.Name, "api", host.APIAddresses(), "raft", host.RaftAddress(),
		"metrics", metricsAddress(host))
	if err := notify("READY=1"); err != nil {
		logger.Warn("systemd", "error", err)
	}

	err = watch(ctx, node, serveErrors, agent.rejoiner.rejoined, logger)
	agent.server.EndStreams() // Shutdown would wait for them
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
func openNode(ctx context.Context, files hostfiles.Files, host hostfiles.Host, credentials hostfiles.Credentials,
	logger *slog.Logger,
) (*raftnode.Node, error) {
	node, err := raftnode.Open(raftnode.Config{
		Dir: files.RaftDir(), ID: host.ID, Bind: host.RaftAddress(), Advertise: host.RaftAddress(),
		Certificate: credentials.Certificate, CA: credentials.CA, Logger: logger,
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
	files       hostfiles.Files
	host        hostfiles.Host
	credentials hostfiles.Credentials
	node        *raftnode.Node
	parts       HostParts
	logger      *slog.Logger
}

// daemon is the agent's parts once newDaemon has built them.
type daemon struct {
	shared
	peers    *api.Peers
	server   *api.Server
	rejoiner *rejoiner
	// the metrics read these parts (serve)
	supervisor *supervisor.Supervisor
	alerts     *controller.Alerts
	volumes    *volumes.Reconciler
	// caughtUp is closed once the state has caught up after the start. The
	// API holds the CLI's calls until then (api.ServerConfig.CaughtUp).
	caughtUp chan struct{}
	// loops are every loop of the agent, in the groups that startLoops
	// starts at different times
	loops loopGroups
}

// loopGroups holds the agent's loops. Each loop is the Run method of a
// value that newDaemon builds, or another method of the same form
// (volumes.Reconciler.Trim, controller.Alerts.RunHeartbeat). A value whose
// fields need setting up (maps, defaults) or that belongs to another
// package comes from its constructor (newX, NewX); a value that only holds
// what it is given is a struct literal.
type loopGroups struct {
	// always start at once, on every host
	always []func(context.Context)
	// controlPlane start at once on a control-plane host: the leader's
	// loops, which do nothing while the host does not lead, and the watch
	// that alerts while there is no leader
	controlPlane []func(context.Context)
	// caughtUp start once the state has caught up with the leader
	// (startLoops)
	caughtUp []func(context.Context)
}

// newDaemon builds the agent's parts and loops but starts nothing. Who
// calls whom:
//
//	supervisor ──wakes (loop.Signal)──▶ stack runner, Traefik
//	volume reconciler ──wakes (loop.Signal)──▶ stack runner
//	stack runner ──wakes (loop.Signal) once a leaver let go──▶ volume reconciler
//	stack runner ──asks (Mounted)──▶ volume reconciler
//	stack runner, Traefik ──call Pause on──▶ supervisor
//	every part that commits ──proposes through──▶ peers (the leader)
//	stack runner, Traefik ──open secrets and keys through──▶ peers (the control plane)
//	API server ──answers for──▶ stack runner (containers), volume reconciler (progress), alerts
//	move steps ──ask──▶ prefetcher, stack runner's files, volume reconciler
//
// The signals come first, so the supervisor does not need the runners. The
// peers come before every part, and the server last, because it answers
// for the parts.
func newDaemon(base shared) *daemon {
	peers := api.NewPeers(hostConfig(base))
	stacksSignal, traefikSignal, volumesSignal := loop.NewSignal(), loop.NewSignal(), loop.NewSignal()
	dockerRestarted := func() {
		stacksSignal.DockerRestarted()
		traefikSignal.DockerRestarted()
	}
	watcher := supervisor.New(base.parts.Docker, restartRules{base.node, base.host.ID}.of, supervisor.DefaultBackoff,
		dockerRestarted, traefikSignal.Now, base.logger)
	stackFiles := stacks.Files{StacksDir: base.files.StacksDir(), SecretsDir: base.files.SecretsDir}
	reconciler := newVolumeReconciler(base, stackFiles.HeldLVs, volumesSignal, stacksSignal, peers.Propose)
	openSecret := newSecretOpener(peers)
	// the runner and the prefetcher write the namespaces' Docker configs
	registries := stacks.NewRegistryConfigs(stackFiles, openSecret)
	runner := newStackRunner(base, stackFiles, watcher, stacksSignal, volumesSignal, registries, reconciler, openSecret,
		peers.Propose)
	prefetcher := stacks.NewPrefetcher(base.host.ID, base.node, base.parts.Docker, registries, base.logger)
	conditions := controller.NewConditions()
	alerts := controller.NewAlerts(base.node, peers.Propose, base.credentials.Decrypt, conditions, base.logger)
	heard := controller.NewHeard()
	rejoiner := newRejoiner(base)
	caughtUp := make(chan struct{})
	joins := &joinLeader{cluster: base.node, propose: peers.Propose, dataKey: base.credentials.DataKey}
	server := newAPIServer(base, peers, runner, reconciler, alerts, heard, joins, rejoiner, caughtUp)

	agent := &daemon{
		shared: base, peers: peers, server: server, rejoiner: rejoiner,
		supervisor: watcher, alerts: alerts, volumes: reconciler, caughtUp: caughtUp,
	}
	agent.loops.always = []func(context.Context){
		networkReconciler{
			cluster: base.node, files: base.files, self: base.host.ID, network: base.parts.Network, logger: base.logger,
		}.Run,
		watcher.Run,
		beatSender{node: base.node, self: base.host.ID, beat: peers.Beat, logger: base.logger}.Run,
	}
	if base.host.Roles.Has(hostrole.ControlPlane) {
		agent.loops.controlPlane = controlPlaneLoops(base, peers, conditions, heard, alerts)
	}
	agent.loops.caughtUp = []func(context.Context){
		newHostReporter(base, peers.Propose, reconciler.Seen).Run,
		newTraefikRunner(base, watcher, traefikSignal, newCertificateKeys(peers)).Run,
		reconciler.Run,
		reconciler.Trim,
		peerForgetter{
			cluster: base.node, self: base.host.ID, storage: base.parts.Storage, propose: peers.Propose,
			logger: base.logger,
		}.Run,
		prefetcher.Run,
		backFromFenceReporter{cluster: base.node, self: base.host.ID, propose: peers.Propose, logger: base.logger}.Run,
		moveSteps{
			cluster: base.node, self: base.host.ID, propose: peers.Propose, imagesReady: prefetcher.Ready,
			stopped: stackFiles.Stopped, released: reconciler.Released, logger: base.logger,
		}.Run,
		runner.Run,
	}
	return agent
}

// controlPlaneLoops are the loops that need the data key. A host without
// the control-plane role never votes, so it never leads, and it holds
// neither the token nor the alert channels' secrets. A worker cut off from
// every control-plane host alerts nobody; the voters see it fail.
func controlPlaneLoops(base shared, peers *api.Peers, conditions *controller.Conditions, heard *controller.Heard,
	alerts *controller.Alerts,
) []func(context.Context) {
	// every host has the same Raft port
	raftAddress := func(id state.HostID) string {
		return net.JoinHostPort(network.NodeAddress(id).String(), strconv.Itoa(base.host.RaftPort))
	}
	knowsLeader := func() bool {
		_, ok := base.node.Leader()
		return ok
	}
	return []func(context.Context){
		controller.NewMembership(base.node, base.node, peers.Propose, raftAddress, base.host.ID, base.logger).Run,
		controller.NewPlacement(base.node, peers.Propose, conditions, base.logger).Run,
		controller.NewMoves(base.node, peers.Propose, peers.Progress, base.logger).Run,
		controller.NewFloatingIP(base.node, peers.Propose, base.credentials.Decrypt, conditions, base.logger).Run,
		controller.NewACME(base.node, peers.Propose, base.credentials.DataKey, base.logger).Run,
		controller.NewFencing(controller.FenceParts{
			Cluster: base.node, Propose: peers.Propose, Open: base.credentials.Decrypt,
			Heard: heard, Ask: peers.AskSilence, Self: base.host.ID, Conditions: conditions, Logger: base.logger,
		}).Run,
		alerts.Run,
		alerts.RunHeartbeat,
		// leading or not: it alerts while there is no leader
		controller.NewQuorumWatch(alerts, base.host.Name, knowsLeader).Run,
	}
}

// hostConfig is what both sides of the API know about this host.
func hostConfig(base shared) api.HostConfig {
	// a nil *datakey.Key in the interface would not compare as nil
	var keys api.Keys
	if base.credentials.DataKey != nil {
		keys = base.credentials.DataKey
	}
	return api.HostConfig{
		Cluster: base.node, CA: base.credentials.CA, Certificate: base.credentials.Certificate,
		Roles: base.host.Roles,
		APIAddress: func(id state.HostID) string {
			// every host has the same API port; tests on 127.0.0.1 have one host
			return net.JoinHostPort(network.NodeAddress(id).String(), strconv.Itoa(base.host.APIPort))
		},
		Keys: keys, HostID: base.host.ID, Version: buildinfo.Version, Logger: base.logger,
	}
}

// newSecretOpener opens the secrets of the stacks that this host runs,
// through what the control plane gives it.
func newSecretOpener(peers *api.Peers) stacks.OpenSecret {
	secrets := &controlPlaneCache[api.HostSecret]{
		what:    "secret",
		fetch:   peers.SecretsOfThisHost,
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

func newCertificateKeys(peers *api.Peers) cachedCertificateKeys {
	return cachedCertificateKeys{cache: &controlPlaneCache[api.HostCertificate]{
		what:    "certificate",
		fetch:   peers.CertificatesOfThisHost,
		version: func(certificate api.HostCertificate) string { return chainDigest(certificate.Chain) },
	}}
}

func (k cachedCertificateKeys) open(ctx context.Context, certificate state.Certificate) ([]byte, error) {
	opened, err := k.cache.get(ctx, certificate.Domain, chainDigest(certificate.Chain))
	return opened.Key, err
}

func (k cachedCertificateKeys) forget() { k.cache.forget() }

func volumeFiles(files hostfiles.Files) volumes.Files {
	return volumes.Files{RecordsDir: files.VolumeRecordsDir(), LastTrim: files.LastTrim()}
}

func newVolumeReconciler(base shared, held func() (map[string]bool, error), signal, stacksSignal *loop.Signal,
	propose controller.Propose,
) *volumes.Reconciler {
	return volumes.New(volumes.Parts{
		Files:       volumeFiles(base.files),
		Host:        base.host.ID,
		Cluster:     base.node,
		Storage:     base.parts.Storage,
		Held:        held,
		Signal:      signal,
		StacksReady: stacksSignal.Now,
		Propose:     propose,
		Logger:      base.logger,
	})
}

func newStackRunner(base shared, files stacks.Files, watcher loop.Pauser, signal, volumesSignal *loop.Signal,
	registries *stacks.RegistryConfigs, volumes stacks.Volumes, openSecret stacks.OpenSecret, propose controller.Propose,
) *stacks.Runner {
	return stacks.New(stacks.Parts{
		Files:           files,
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

func newAPIServer(base shared, peers *api.Peers, runner *stacks.Runner, volumes api.Volumes,
	alerts *controller.Alerts, heard *controller.Heard, joins api.Joins, recover api.RecoverHost,
	caughtUp <-chan struct{},
) *api.Server {
	return api.NewServer(api.ServerConfig{
		HostConfig: hostConfig(base), Peers: peers,
		Containers: runner, Volumes: volumes, Alerts: alertsAPI{alerts}, Beats: heard,
		Joins: joins, Recover: recover, CaughtUp: caughtUp,
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

// startLoops starts the agent's loops. The loops of the group caughtUp wait
// because until the state has caught up, it may still place a stack here
// that has already moved to another host: no container may start yet, and
// no volume of that stack may become primary here. The report waits too,
// because it compares the host with the state.
func (d *daemon) startLoops(ctx context.Context, loops *sync.WaitGroup) {
	start := func(group []func(context.Context)) {
		for _, run := range group {
			loops.Go(func() { run(ctx) })
		}
	}
	start(d.loops.always)
	start(d.loops.controlPlane)
	loops.Go(func() {
		if !waitForState(ctx, d.node, d.peers, d.logger) {
			return
		}
		close(d.caughtUp)
		// this runs as one of the loops, so loops.Wait cannot have returned
		// yet: adding to it from here is allowed
		start(d.loops.caughtUp)
	})
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
func waitForState(ctx context.Context, node *raftnode.Node, peers *api.Peers, logger *slog.Logger) bool {
	for ctx.Err() == nil {
		catchUp, cancel := context.WithTimeout(ctx, catchUpTimeout)
		err := node.CatchUp(catchUp)
		if err == nil {
			err = peers.CatchUpWithLeader(catchUp)
		}
		cancel()
		if err == nil {
			logger.Info("the state caught up with the leader; starting the stacks")
			return true
		}
		logger.Warn("the state has not caught up with the leader yet", "error", err)
		select {
		case <-ctx.Done():
		case <-time.After(catchUpPause):
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
// systemd restarts a deadlocked agent. It ends the agent after a rejoin,
// which takes effect at the next start.
func watch(ctx context.Context, node *raftnode.Node, serveErrors <-chan error, rejoined <-chan struct{},
	logger *slog.Logger,
) error {
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
		case <-rejoined:
			return ErrRejoining
		case <-tick:
			node.Read(func(*state.State) {}) // hangs if the state's lock does
			if err := notify("WATCHDOG=1"); err != nil {
				logger.Warn("systemd", "error", err)
			}
		}
	}
}
