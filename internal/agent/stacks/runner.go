// Package stacks is the agent's stack runner and its prefetcher
// (docs/developing/host.md). This file holds the pass. apply.go runs one
// stack, and leavers.go stops the stacks that left.
package stacks

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/webcodingcat/clowder/internal/agent/loop"
	"github.com/webcodingcat/clowder/internal/clusterspec"
	"github.com/webcodingcat/clowder/internal/docker"
	"github.com/webcodingcat/clowder/internal/state"
)

// Docker is what the runner needs from Docker.
type Docker interface {
	// ComposeUp and ComposePull pull with the logins of the Docker config
	// directory config (registryConfigDir).
	ComposeUp(ctx context.Context, config, stack string, files, leftOut []string) error
	ComposePull(ctx context.Context, config, stack string, files, services []string) error
	ComposeDown(ctx context.Context, stack, file string) error
	ComposePs(ctx context.Context, stack, file string) (string, error)
	ComposeLogs(ctx context.Context, stack, file string, follow bool, out io.Writer) error
	Containers(ctx context.Context, label string) ([]docker.Container, error)
}

// Volumes is what the runner needs from the volume reconciler. Mounted
// returns the mount point once the copy is ready for a stack, an error if
// the copy cannot get there, and neither while it is on its way.
type Volumes interface {
	Mounted(lv string) (dir string, ready bool, err error)
}

// Files are where the runner keeps what it knows about the stacks on this
// host.
type Files struct {
	// StacksDir has one directory per stack, named by its Compose project
	// (names.Project), with its rendered Compose file and the LVs it mounts
	StacksDir string
	// SecretsDir holds the stacks' secrets in plain text, on tmpfs.
	// clowder's own projects keep their secrets there too.
	SecretsDir string
}

func (f Files) stackDir(project string) string { return filepath.Join(f.StacksDir, project) }

func (f Files) stackFile(project string) string {
	return filepath.Join(f.stackDir(project), "compose.yaml")
}

func (f Files) stackVolumesFile(project string) string {
	return filepath.Join(f.stackDir(project), "volumes.json")
}

func (f Files) secretsDir(project string) string { return filepath.Join(f.SecretsDir, project) }

func (f Files) secretFilesDir(project string) string {
	return filepath.Join(f.secretsDir(project), "files")
}

func (f Files) secretEnvFile(project string) string {
	return filepath.Join(f.secretsDir(project), "env.yaml")
}

func (f Files) secretFile(project, name, key string) string {
	return filepath.Join(f.secretFilesDir(project), name, key)
}

// Stopped reports whether a stack (by its project) has no containers on
// this host: it never ran here, or its compose down succeeded
// (stopLeavers).
func (f Files) Stopped(project string) (bool, error) {
	_, err := os.Stat(f.stackFile(project))
	switch {
	case errors.Is(err, os.ErrNotExist):
		return true, nil
	case err != nil:
		return false, fmt.Errorf("look for the stack %s: %w", project, err)
	}
	return false, nil
}

// HeldLVs returns the LVs that the containers of the stacks on this host
// may hold, including leavers that have not stopped yet. A list that cannot
// be read is an error, because then any LV may be held.
func (f Files) HeldLVs() (map[string]bool, error) {
	entries, err := os.ReadDir(f.StacksDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("list the stacks of this host: %w", err)
	}
	held := map[string]bool{}
	for _, entry := range entries {
		project := entry.Name()
		if _, err := os.Stat(f.stackFile(project)); errors.Is(err, os.ErrNotExist) {
			continue // stopped
		}
		lvs, err := f.readMountedLVs(project)
		if err != nil {
			return nil, err
		}
		for _, lv := range lvs {
			held[lv] = true
		}
	}
	return held, nil
}

// Parts are what the agent gives the runner to work with.
type Parts struct {
	Files Files
	// NodeAddress is where published ports listen.
	Host        state.HostID
	NodeAddress netip.Addr
	Cluster     loop.Cluster
	Docker      Docker
	Volumes     Volumes
	// OpenSecret gives a Secret's values by key, as the state names it
	OpenSecret OpenSecret
	// Supervisor is paused while compose up or down changes a stack's
	// containers
	Supervisor loop.Pauser
	// Signal is how the supervisor and the volume reconciler start a pass,
	// and how the supervisor reports that Docker restarted
	Signal *loop.Signal
	// Registries writes the Docker configs that the stacks pull with. The
	// prefetcher shares it.
	Registries *RegistryConfigs
	// VolumesReleased starts a pass of the volume reconciler once a stack
	// that left this host no longer holds its copies. If it is nil, no pass
	// starts.
	VolumesReleased func()
	// Propose commits a command (a controller.Propose, spelled out as in
	// loop.CommitAsHost)
	Propose func(context.Context, state.Command) (uint64, error)
	Logger  *slog.Logger
}

// Runner runs the stacks that the state places on this host. It keeps its
// parts in unexported fields, so only New sets them and nothing changes
// them while the runner's loop runs.
type Runner struct {
	files       Files
	host        state.HostID
	nodeAddress netip.Addr
	cluster     loop.Cluster
	docker      Docker
	volumes     Volumes
	openSecret  OpenSecret
	supervisor  loop.Pauser
	signal      *loop.Signal
	registries  *RegistryConfigs
	// volumesReleased is Parts.VolumesReleased
	volumesReleased func()
	propose         func(context.Context, state.Command) (uint64, error)
	logger          *slog.Logger

	// jobs counts the running jobs, so that Run returns only after they end
	jobs sync.WaitGroup
	// mu guards the fields that the pass and the jobs share.
	mu sync.Mutex
	// applied holds what each stack last came up with (by project). It
	// starts empty, so every stack comes up again after the agent starts:
	// after a boot nothing runs (restart: "no").
	applied map[string]loop.BroughtUp
	// busy holds the projects of the stacks that have a job running. stale
	// holds those of them that a pass skipped, so that the job starts
	// another pass when it ends.
	busy, stale map[string]bool
}

// New creates the runner but starts nothing.
func New(parts Parts) *Runner {
	return &Runner{
		files: parts.Files, host: parts.Host, nodeAddress: parts.NodeAddress, cluster: parts.Cluster,
		docker: parts.Docker, volumes: parts.Volumes, openSecret: parts.OpenSecret,
		supervisor: parts.Supervisor, signal: parts.Signal, registries: parts.Registries,
		volumesReleased: parts.VolumesReleased,
		propose:         parts.Propose, logger: parts.Logger,
		applied: map[string]loop.BroughtUp{}, busy: map[string]bool{}, stale: map[string]bool{},
	}
}

type desiredState struct {
	config clusterspec.Config
	// stacks are keyed by their Compose project (Stack.Project), the name
	// of everything the agent keeps per stack
	stacks  map[string]*state.Stack
	volumes map[string]*state.Volume
	secrets map[string]*state.Secret
	// copied is the copy that all of this comes from. The registry logins
	// are read from it (RegistryConfigs.Write).
	copied *state.State
}

// readDesired reads the shared copy of the state, so that the pass works
// outside the lock that Raft's Apply waits for.
func readDesired(cluster loop.Cluster, self state.HostID) desiredState {
	copied := cluster.SharedCopy()
	desired := desiredState{
		config: copied.Config.Spec, stacks: map[string]*state.Stack{}, volumes: copied.Volumes,
		secrets: copied.Secrets, copied: copied,
	}
	// a host that was fenced runs nothing until it is active again, because
	// its stacks may run elsewhere by now; a stack that stops for a move is
	// a leaver here (stopLeavers)
	for _, stack := range copied.StacksToRun(self) {
		desired.stacks[stack.Project()] = stack
	}
	return desired
}

// Run runs passes until ctx ends (loop.Run), then waits for the jobs.
func (r *Runner) Run(ctx context.Context) {
	loop.Run(ctx, r.cluster, r.signal.Wake(), func() { r.pass(ctx) })
	r.jobs.Wait()
}

// pass stops the leavers first, so that a volume that passes from one
// stack to another is never used by both at once: two databases on one
// file system would ruin it. Each stack then runs in a job of its own, so a
// stack whose pull or compose up hangs holds up no other stack.
func (r *Runner) pass(ctx context.Context) {
	desired := readDesired(r.cluster, r.host)
	inUse := r.stopLeavers(ctx, desired)
	for _, project := range slices.Sorted(maps.Keys(desired.stacks)) {
		stack := desired.stacks[project]
		// the jobs share desired and inUse, which nothing changes after this
		r.start(project, func() { r.run(ctx, desired, stack, inUse) })
	}
	r.removeSecretsOfOthers(desired)
	r.removeRegistryConfigsOfOthers(desired)
}

// start runs a job for a stack in a separate goroutine, unless a job for
// the stack is already running. In that case the running job starts another
// pass when it ends, and that pass sees the state as it is by then.
func (r *Runner) start(project string, job func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.busy[project] {
		r.stale[project] = true
		return
	}
	r.busy[project] = true
	r.jobs.Go(func() {
		job()
		r.mu.Lock()
		again := r.stale[project]
		delete(r.busy, project)
		delete(r.stale, project)
		r.mu.Unlock()
		if again {
			r.signal.Now()
		}
	})
}

// markStaleIfBusy marks a stack with a running job stale, so that the job
// starts another pass when it ends.
func (r *Runner) markStaleIfBusy(project string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.busy[project] {
		r.stale[project] = true
	}
	return r.busy[project]
}

func (r *Runner) run(ctx context.Context, desired desiredState, stack *state.Stack, inUse volumesInUse) {
	err := r.apply(ctx, desired, stack, inUse)
	switch {
	case errors.Is(err, errVolumeInUse), errors.Is(err, errVolumeNotReady):
		// the stack waits, pass after pass, until the stop succeeds or the
		// volume is mounted
		r.logger.Info("a stack waits for its volume", "stack", stack.ID(), "reason", err)
	case err != nil && ctx.Err() != nil:
		// stopping: the error says nothing about the stack
	case err != nil:
		r.logger.Error("run a stack", "stack", stack.ID(), "error", err)
		r.reportFailing(ctx, stack, err)
	}
}

func (r *Runner) appliedWith(project string) loop.BroughtUp {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.applied[project]
}

func (r *Runner) setApplied(project string, up loop.BroughtUp) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.applied[project] = up
}

func (r *Runner) forgetApplied(project string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.applied, project)
}
