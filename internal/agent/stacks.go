package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"slices"

	"github.com/webcodingcat/clowder/internal/clusterspec"
	"github.com/webcodingcat/clowder/internal/controller"
	"github.com/webcodingcat/clowder/internal/docker"
	"github.com/webcodingcat/clowder/internal/state"
	"github.com/webcodingcat/clowder/internal/storage"
	"github.com/webcodingcat/clowder/internal/supervisor"
)

// Docker is what the agent needs from Docker; docker.Client is the real one.
type Docker interface {
	supervisor.Docker
	ComposeUp(ctx context.Context, stack string, files, leftOut []string) error
	ComposePull(ctx context.Context, stack string, files, services []string) error
	ComposeDown(ctx context.Context, stack, file string) error
	ComposePs(ctx context.Context, stack, file string) (string, error)
	ComposeLogs(ctx context.Context, stack, file string, follow bool, out io.Writer) error
	Login(ctx context.Context, registry, username, password string) error
	Containers(ctx context.Context, label string) ([]docker.Container, error)
	Inspect(ctx context.Context, id string) (docker.Details, error)
	Ping(ctx context.Context) error
}

// Storage is what the agent needs from the storage driver; storage.Driver is
// the real one.
type Storage interface {
	Ensure(ctx context.Context, volume storage.Volume) error
	Remove(ctx context.Context, volume storage.Volume) error
	Trim(ctx context.Context, volumes []storage.Volume) error
	PoolUsage(ctx context.Context, pool string) (sizeBytes int64, dataPercent, metadataPercent float64, err error)
	MountPoint(volume storage.Volume) string
}

// stateReader is what the agent's loops need from the Raft member. A loop
// reads the state through SharedCopy and changes nothing of it.
type stateReader interface {
	Read(use func(current *state.State))
	SharedCopy() (*state.State, error)
	Changed() <-chan struct{}
}

// stackRunner runs the stacks the state places on this host. A pass is in
// this file; how it runs one stack is in stack_apply.go, how it stops the
// stacks that left in leavers.go, and the volume copies are in volumes.go
// and records.go.
type stackRunner struct {
	files   Files
	host    Host
	cluster stateReader
	docker  Docker
	storage Storage
	open    func(sealed state.Sealed) ([]byte, error)
	// digestKey keys the labels of secret files (secretLabelPurpose)
	digestKey []byte
	logger    *slog.Logger
	// supervisor is paused while compose up or down changes a stack's
	// containers
	supervisor pauser
	// signal is how the supervisor starts a pass and says Docker restarted
	signal  *passSignal
	propose controller.Propose

	// Only the runner's loop (run) uses what follows, so it needs no lock.
	// trim runs in a loop of its own, so it must use only the fields above
	// and the record files.

	// applied is what each stack last came up with (by project). It starts
	// empty, so every stack comes up again after the agent started: after
	// a boot nothing runs (restart: "no").
	applied map[string]broughtUp
	// logins holds a hash of each registry's login, so the agent logs in
	// again only when it changed
	logins map[string][32]byte
	// unexplained are the LVs of copies kept without a reason, warned
	// about once
	unexplained map[string]bool
}

// desiredState is what this host should run, from a copy of the state.
type desiredState struct {
	config clusterspec.Config
	// stacks are by their Compose project (Stack.Project), the name of all
	// the agent keeps per stack
	stacks     map[string]*state.Stack
	volumes    map[string]*state.Volume
	secrets    map[string]*state.Secret
	hostNames  map[state.HostID]string
	registries map[string]state.RegistryLogin
	// retired are the copies the cluster gave up, on any host
	retired []state.RetiredCopy
}

// readDesired reads what this host should run: a copy of the state,
// made under the lock Raft's Apply waits for, which a pass then works on
// outside it.
func readDesired(cluster stateReader, self state.HostID) (desiredState, error) {
	copied, err := cluster.SharedCopy()
	if err != nil {
		return desiredState{}, fmt.Errorf("copy the state: %w", err)
	}
	desired := desiredState{
		config: copied.Config.Spec, stacks: map[string]*state.Stack{}, volumes: copied.Volumes,
		secrets: copied.Secrets, hostNames: map[state.HostID]string{}, registries: copied.RegistryLogins(),
		retired: copied.Retired,
	}
	for _, stack := range copied.Stacks {
		if stack.Host != nil && *stack.Host == self {
			desired.stacks[stack.Project()] = stack
		}
	}
	for id, host := range copied.Hosts {
		desired.hostNames[id] = host.Name
	}
	return desired, nil
}

// run runs passes until ctx ends (runPasses).
func (r *stackRunner) run(ctx context.Context) {
	runPasses(ctx, r.cluster, r.signal.wake, func() { r.pass(ctx) })
}

// pass brings this host once to what the state says. First it stops the
// stacks that left this host, then it brings up the others. A volume that
// passes from one stack to another must never be used by both at once:
// two databases on one file system would ruin it. If a stack fails to stop,
// it may still use its volumes, and a stack that names one of them waits
// for the next pass. A stack that fails to come up is reported failing.
func (r *stackRunner) pass(ctx context.Context) {
	desired, err := readDesired(r.cluster, r.host.ID)
	if err != nil {
		r.logger.Error("read the state", "error", err)
		return
	}
	r.logIn(ctx, desired)
	inUse := r.stopLeavers(ctx, desired)
	for _, project := range slices.Sorted(maps.Keys(desired.stacks)) {
		stack := desired.stacks[project]
		err := r.apply(ctx, desired, stack, inUse)
		switch {
		case errors.Is(err, errVolumeInUse): // it waits, at every pass until the stop works
			r.logger.Info("a stack waits for its volume", "stack", stack.ID(), "reason", err)
		case err != nil && ctx.Err() != nil:
			return // stopping: the error says nothing about the stack
		case err != nil:
			r.logger.Error("run a stack", "stack", stack.ID(), "error", err)
			r.reportFailing(ctx, stack, err)
		}
	}
	r.removeVolumes(ctx, desired)
	r.removeSecretsOfOthers(desired)
}
