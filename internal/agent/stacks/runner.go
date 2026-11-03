// Package stacks is the agent's stack runner: it runs the stacks the state
// places on this host with docker compose, writes their secrets, and stops
// the stacks that left. The volume reconciler (agent/volumes) keeps their
// volume copies; the runner waits until those are mounted. A pass is in
// this file; how it runs one stack is in apply.go, how it stops the stacks
// that left in leavers.go.
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

	"github.com/webcodingcat/clowder/internal/agent/loop"
	"github.com/webcodingcat/clowder/internal/clusterspec"
	"github.com/webcodingcat/clowder/internal/docker"
	"github.com/webcodingcat/clowder/internal/state"
)

// Docker is what the runner needs from Docker; docker.Client is the real
// one.
type Docker interface {
	ComposeUp(ctx context.Context, stack string, files, leftOut []string) error
	ComposePull(ctx context.Context, stack string, files, services []string) error
	ComposeDown(ctx context.Context, stack, file string) error
	ComposePs(ctx context.Context, stack, file string) (string, error)
	ComposeLogs(ctx context.Context, stack, file string, follow bool, out io.Writer) error
	Login(ctx context.Context, registry, username, password string) error
	Containers(ctx context.Context, label string) ([]docker.Container, error)
}

// Volumes is what the runner needs from the volume reconciler;
// volumes.Reconciler is the real one. Mounted tells about this host's copy
// of a volume, by its LV: its mount point once it is ready for a stack, an
// error if it cannot get there, neither while it is on its way.
type Volumes interface {
	Mounted(lv string) (dir string, ready bool, err error)
}

// Files are where the runner keeps what it knows of the stacks on this
// host; the agent's files say where each is.
type Files struct {
	// StacksDir has a directory per stack, by its Compose project
	// (names.Project): its rendered Compose file and the LVs it mounts
	StacksDir string
	// SecretsDir holds the stacks' secrets in clear, on tmpfs; clowder's
	// own projects keep theirs beside them
	SecretsDir string
}

func (f Files) stackDir(project string) string { return filepath.Join(f.StacksDir, project) }

func (f Files) stackFile(project string) string {
	return filepath.Join(f.stackDir(project), "compose.yaml")
}

func (f Files) stackVolumesFile(project string) string {
	return filepath.Join(f.stackDir(project), "volumes.json")
}

// The files of a stack's secrets, by its project: a file per secret a
// service asks for as a file (bind-mounted to /run/secrets/<name>), and the
// second Compose file with the secret variables.
func (f Files) secretsDir(project string) string { return filepath.Join(f.SecretsDir, project) }

func (f Files) secretFilesDir(project string) string {
	return filepath.Join(f.secretsDir(project), "files")
}

func (f Files) secretEnvFile(project string) string {
	return filepath.Join(f.secretsDir(project), "env.yaml")
}

func (f Files) secretFile(project, name string) string {
	return filepath.Join(f.secretFilesDir(project), name)
}

// Stopped tells whether a stack (by its project) has no containers here:
// it never ran here, or its compose down worked (stopLeavers).
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

// HeldLVs are the LVs that the containers of the stacks on this host may
// hold: those in the volumes.json of each stack that has a compose.yaml,
// placed here or a leaver that has not stopped yet. The volume reconciler
// leaves such a copy as it is. A list that does not read is an error: then
// any LV may be held.
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

// Parts are what the runner works with, from the agent.
type Parts struct {
	Files Files
	// Host is this host's ID; NodeAddress its address in the overlay,
	// where published ports listen
	Host        state.HostID
	NodeAddress netip.Addr
	Cluster     loop.Cluster
	Docker      Docker
	Volumes     Volumes
	// Open unseals the secrets' values (datakey.Key.Open)
	Open func(sealed state.Sealed) ([]byte, error)
	// DigestKey keys the labels of secret files, derived from the data key
	// for SecretLabelPurpose
	DigestKey []byte
	// Supervisor is paused while compose up or down changes a stack's
	// containers
	Supervisor loop.Pauser
	// Signal is how the supervisor and the volume reconciler start a pass,
	// and how the supervisor says Docker restarted
	Signal *loop.Signal
	// Propose commits a command (a controller.Propose, spelled out as in
	// loop.CommitAsHost)
	Propose func(context.Context, state.Command) (uint64, error)
	Logger  *slog.Logger
}

// Runner runs the stacks the state places on this host. It keeps its parts
// in fields of its own, unexported, so nothing but New sets them and no
// one changes them while the runner's loop runs.
type Runner struct {
	files       Files
	host        state.HostID
	nodeAddress netip.Addr
	cluster     loop.Cluster
	docker      Docker
	volumes     Volumes
	open        func(sealed state.Sealed) ([]byte, error)
	digestKey   []byte
	supervisor  loop.Pauser
	signal      *loop.Signal
	propose     func(context.Context, state.Command) (uint64, error)
	logger      *slog.Logger

	// Only the runner's loop (Run) uses what follows, so it needs no lock.

	// applied is what each stack last came up with (by project). It starts
	// empty, so every stack comes up again after the agent started: after
	// a boot nothing runs (restart: "no").
	applied map[string]loop.BroughtUp
	// logins holds a hash of each registry's login, so the agent logs in
	// again only when it changed
	logins map[string][32]byte
}

// New makes the runner; it starts nothing.
func New(parts Parts) *Runner {
	return &Runner{
		files: parts.Files, host: parts.Host, nodeAddress: parts.NodeAddress, cluster: parts.Cluster,
		docker: parts.Docker, volumes: parts.Volumes, open: parts.Open, digestKey: parts.DigestKey,
		supervisor: parts.Supervisor, signal: parts.Signal, propose: parts.Propose, logger: parts.Logger,
		applied: map[string]loop.BroughtUp{}, logins: map[string][32]byte{},
	}
}

// desiredState is what this host should run, from a copy of the state.
type desiredState struct {
	config clusterspec.Config
	// stacks are by their Compose project (Stack.Project), the name of all
	// the agent keeps per stack
	stacks     map[string]*state.Stack
	volumes    map[string]*state.Volume
	secrets    map[string]*state.Secret
	registries map[string]state.RegistryLogin
}

// readDesired reads what this host should run: a copy of the state,
// made under the lock Raft's Apply waits for, which a pass then works on
// outside it.
func readDesired(cluster loop.Cluster, self state.HostID) (desiredState, error) {
	copied, err := cluster.SharedCopy()
	if err != nil {
		return desiredState{}, fmt.Errorf("copy the state: %w", err)
	}
	desired := desiredState{
		config: copied.Config.Spec, stacks: map[string]*state.Stack{}, volumes: copied.Volumes,
		secrets: copied.Secrets, registries: copied.RegistryLogins(),
	}
	for _, stack := range copied.Stacks {
		// a stack that stops for a move is a leaver here (stopLeavers)
		if stack.Host != nil && *stack.Host == self && !copied.StopsForMove(stack.ID()) {
			desired.stacks[stack.Project()] = stack
		}
	}
	return desired, nil
}

// Run runs passes until ctx ends (loop.Run).
func (r *Runner) Run(ctx context.Context) {
	loop.Run(ctx, r.cluster, r.signal.Wake(), func() { r.pass(ctx) })
}

// pass brings this host once to what the state says. First it stops the
// stacks that left this host, then it brings up the others. A volume that
// passes from one stack to another must never be used by both at once:
// two databases on one file system would ruin it. If a stack fails to stop,
// it may still use its volumes, and a stack that names one of them waits
// for the next pass; so does a stack whose volume is not mounted yet. A
// stack that fails to come up is reported failing.
func (r *Runner) pass(ctx context.Context) {
	desired, err := readDesired(r.cluster, r.host)
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
		case errors.Is(err, errVolumeInUse), errors.Is(err, errVolumeNotReady):
			// it waits, at every pass until the stop worked or the volume
			// is mounted
			r.logger.Info("a stack waits for its volume", "stack", stack.ID(), "reason", err)
		case err != nil && ctx.Err() != nil:
			return // stopping: the error says nothing about the stack
		case err != nil:
			r.logger.Error("run a stack", "stack", stack.ID(), "error", err)
			r.reportFailing(ctx, stack, err)
		}
	}
	r.removeSecretsOfOthers(desired)
}
