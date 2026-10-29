package agent

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/webcodingcat/clowder/internal/atomicfile"
	"github.com/webcodingcat/clowder/internal/clusterdir"
	"github.com/webcodingcat/clowder/internal/compose"
	"github.com/webcodingcat/clowder/internal/network"
	"github.com/webcodingcat/clowder/internal/state"
	"github.com/webcodingcat/clowder/internal/storage"
	"github.com/webcodingcat/clowder/internal/supervisor"
)

// Docker is what the agent needs from Docker; docker.Client is the real one.
type Docker interface {
	supervisor.Docker
	ComposeUp(ctx context.Context, stack string, files []string) error
	ComposeDown(ctx context.Context, stack, file string) error
	ComposePs(ctx context.Context, stack, file string) (string, error)
	ComposeLogs(ctx context.Context, stack, file string, follow bool, out io.Writer) error
	Login(ctx context.Context, registry, username, password string) error
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

// stateReader is what the agent's loops need from the Raft member.
type stateReader interface {
	Read(use func(current *state.State))
	Changed() <-chan struct{}
}

// The files of the stacks on this host: per stack its rendered Compose file
// and the record of its volume copies.
func (f Files) stacksDir() string            { return filepath.Join(f.Dir, "stacks") }
func (f Files) stackDir(name string) string  { return filepath.Join(f.stacksDir(), name) }
func (f Files) stackFile(name string) string { return filepath.Join(f.stackDir(name), "compose.yaml") }

func (f Files) volumesRecord(name string) string {
	return filepath.Join(f.stackDir(name), "volumes.json")
}

func (f Files) lastTrim() string { return filepath.Join(f.Dir, "storage", "last-trim") }

// stackRunner runs the stacks the state places on this host.
type stackRunner struct {
	files   Files
	host    Host
	version string
	cluster stateReader
	docker  Docker
	storage Storage
	open    func(sealed state.Sealed) ([]byte, error)
	// digestKey keys the digests of secret files (secretDigestPurpose)
	digestKey []byte
	logger    *slog.Logger
	// supervisor and propose need the runner first (the supervisor its
	// rules, the API server its containers), so Run sets them afterwards
	supervisor *supervisor.Supervisor
	propose    func(ctx context.Context, cmd state.Command) (uint64, error)

	mu sync.Mutex
	// applied is the hash of the Compose file each stack last came up with.
	// It starts empty, and empties again when Docker restarted, so every
	// stack comes up again: after a boot nothing runs (restart: "no").
	applied map[string][32]byte
	// logins holds a hash of each registry's login, so the agent logs in
	// again only when it changed
	logins map[string][32]byte
	// wake starts a pass at once (buffered, 1): after Docker restarted,
	// nothing runs until the next pass
	wake chan struct{}
}

// desiredState is what this host should run, from a copy of the state.
type desiredState struct {
	config     clusterdir.Config
	stacks     map[string]*state.Stack
	volumes    map[string]*state.Volume
	hostNames  map[state.HostID]string
	registries map[string]state.Sealed
	self       state.Host
}

func (r *stackRunner) read() (desiredState, error) {
	var copied *state.State
	var err error
	r.cluster.Read(func(current *state.State) { copied, err = current.Copy() })
	if err != nil {
		return desiredState{}, err
	}
	desired := desiredState{
		config: copied.Config.Spec, stacks: map[string]*state.Stack{}, volumes: copied.Volumes,
		hostNames: map[state.HostID]string{}, registries: copied.Registries,
	}
	for name, stack := range copied.Stacks {
		if stack.Host != nil && *stack.Host == r.host.ID {
			desired.stacks[name] = stack
		}
	}
	for id, host := range copied.Hosts {
		desired.hostNames[id] = host.Name
	}
	if self, ok := copied.Hosts[r.host.ID]; ok {
		desired.self = *self
	}
	return desired, nil
}

// reconcile runs a pass at every change of the state and every
// reconcileInterval, until ctx ends.
func (r *stackRunner) reconcile(ctx context.Context) {
	for {
		// take the channel before reading, so no change between goes unseen
		changed := r.cluster.Changed()
		r.pass(ctx)
		select {
		case <-ctx.Done():
			return
		case <-changed:
		case <-r.wake:
		case <-time.After(reconcileInterval):
		}
	}
}

// pass brings this host once to what the state says.
func (r *stackRunner) pass(ctx context.Context) {
	desired, err := r.read()
	if err != nil {
		r.logger.Error("read the state", "error", err)
		return
	}
	if err := r.reportHost(ctx, desired); err != nil {
		r.logger.Error("report this host", "error", err)
	}
	r.logIn(ctx, desired)
	for _, name := range slices.Sorted(maps.Keys(desired.stacks)) {
		if err := r.apply(ctx, desired, desired.stacks[name]); err != nil {
			r.logger.Error("run a stack", "stack", name, "error", err)
		}
	}
	r.removeOthers(ctx, desired)
}

// forgetApplied makes every stack come up again in a pass it starts at
// once; the supervisor calls it when Docker's events broke off (Docker
// restarted).
func (r *stackRunner) forgetApplied() {
	r.mu.Lock()
	clear(r.applied)
	r.mu.Unlock()
	select {
	case r.wake <- struct{}{}:
	default: // a pass is due already
	}
}

// apply runs one stack: its volumes, its rendered Compose file, compose up
// unless that file came up before, then the status.
func (r *stackRunner) apply(ctx context.Context, desired desiredState, stack *state.Stack) error {
	parsed, err := compose.Parse(ctx, stack.Name, []byte(stack.Source))
	if err != nil {
		return err
	}
	secrets, err := r.openSecrets(parsed, stack.Secrets)
	if err != nil {
		return err
	}
	volumes := map[string]storage.Volume{}
	mounted := map[string]compose.MountedVolume{}
	for name := range parsed.Volumes {
		volume, err := r.describe(desired, stack.Name+"/"+name)
		if err != nil {
			return err
		}
		volumes[name] = volume
		mounted[name] = compose.MountedVolume{
			Dir: r.storage.MountPoint(volume), Generation: desired.volumes[volume.ID].Generation,
		}
	}
	// rendered before any volume is made: a stack that cannot run (no
	// Floating IP before the first cluster apply) leaves nothing behind
	// cluster.yaml's check made sure it parses; before the first apply it is
	// empty, the zero address, which Render refuses
	floatingIP, _ := netip.ParseAddr(desired.config.Ingress.FloatingIP)
	rendered, err := compose.Render(parsed, compose.Target{
		FloatingIP: floatingIP, NodeAddress: r.host.NodeAddress, Resolver: network.Resolver,
		Volumes: mounted, SecretsDir: r.files.secretFilesDir(stack.Name), SecretDigests: secrets.digests,
	})
	if err != nil {
		return err
	}
	// recorded before they exist, so a copy made by a crashed pass is still
	// found and removed later
	if err := r.record(stack.Name, slices.Collect(maps.Values(volumes))); err != nil {
		return err
	}
	for _, volume := range volumes {
		if err := r.storage.Ensure(ctx, volume); err != nil {
			return err
		}
	}
	if err := r.up(ctx, stack.Name, rendered, secrets); err != nil {
		return err
	}
	if stack.Status == state.StackRunning {
		return nil
	}
	return r.report(ctx, state.KindSetStackStatus, state.SetStackStatus{
		Name: stack.Name, Status: state.StackRunning, Host: stack.Host, Stopped: stack.Stopped,
		Generation: stack.Generation,
	})
}

// up writes a stack's Compose file and secrets and runs compose up, unless
// all of it came up before. A failed compose up is tried again at the next
// pass.
func (r *stackRunner) up(ctx context.Context, name string, rendered []byte, secrets stackSecrets) error {
	hash := upHash(rendered, secrets)
	r.mu.Lock()
	applied, ok := r.applied[name]
	r.mu.Unlock()
	if ok && applied == hash {
		return nil
	}
	file := r.files.stackFile(name)
	if err := atomicfile.Write(file, rendered, 0o600); err != nil {
		return err
	}
	if err := r.writeSecrets(name, secrets); err != nil {
		return err
	}
	files := []string{file}
	if secrets.env != nil {
		files = append(files, r.files.secretEnvFile(name))
	}
	release := r.supervisor.Hold(name)
	err := r.docker.ComposeUp(ctx, name, files)
	release()
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.applied[name] = hash
	r.mu.Unlock()
	return r.removeStaleSecrets(name, secrets)
}

// upHash covers everything compose up reads: the rendered file, the secret
// variables and the secret files.
func upHash(rendered []byte, secrets stackSecrets) [32]byte {
	hash := sha256.New()
	writeParts(hash, rendered, secrets.env)
	for _, name := range slices.Sorted(maps.Keys(secrets.files)) {
		writeParts(hash, []byte(name), secrets.files[name])
	}
	return [32]byte(hash.Sum(nil))
}

// describe turns a volume of the state into this host's copy of it, as the
// storage driver takes it. The LV's name carries the generation, so a
// restored volume is a new LV next to the old one; the DRBD resource has the
// LV's name.
func (r *stackRunner) describe(desired desiredState, id string) (storage.Volume, error) {
	volume, ok := desired.volumes[id]
	if !ok {
		return storage.Volume{}, fmt.Errorf("volume %s is not placed yet", id)
	}
	index := slices.IndexFunc(volume.Placements, func(p state.Placement) bool { return p.Host == r.host.ID })
	if index < 0 {
		return storage.Volume{}, fmt.Errorf("volume %s has no copy on this host", id)
	}
	stackName, name, _ := strings.Cut(id, "/")
	lv := compose.VolumeName(stackName, name, volume.Generation)
	pool := desired.config.StorageClasses[volume.Placements[index].Class].Pool
	described := storage.Volume{ID: id, LV: lv, Pool: pool, SizeBytes: volume.SizeBytes}
	if volume.Minor == nil {
		return described, nil // replicas 0: a plain LV
	}
	resource := &storage.Resource{Name: lv, Minor: *volume.Minor, ResyncRate: desired.config.Replication.ResyncRate}
	for _, placement := range volume.Placements {
		node := storage.Node{Host: desired.hostNames[placement.Host], ID: placement.Host}
		if !placement.Tiebreaker {
			group, _, _ := strings.Cut(desired.config.StorageClasses[placement.Class].Pool, "/")
			node.Disk = "/dev/" + group + "/" + lv
		}
		resource.Nodes = append(resource.Nodes, node)
	}
	described.Resource = resource
	return described, nil
}

// removeOthers stops the stacks that left this host and removes the volume
// copies the state no longer places here. A stack's directory goes once
// nothing of it is left on this host.
//
// The files tell how far a leaver got, so a crash at any point is picked up:
// its compose.yaml exists until compose down worked (stop), its
// volumes.json lists the copies still here (removeVolumes), and its
// directory goes with the last copy. Its secrets go once compose.yaml is
// gone (removeSecretsOfOthers).
func (r *stackRunner) removeOthers(ctx context.Context, desired desiredState) {
	entries, err := os.ReadDir(r.files.stacksDir())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		r.logger.Error("list the stacks of this host", "error", err)
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		_, staying := desired.stacks[name]
		if !staying {
			if err := r.stop(ctx, name); err != nil {
				r.logger.Error("stop a stack that left this host", "stack", name, "error", err)
				continue
			}
		}
		left, err := r.removeVolumes(ctx, desired, name)
		if err != nil {
			r.logger.Error("remove the volumes of a stack", "stack", name, "error", err)
			continue
		}
		if !staying && left == 0 {
			if err := os.RemoveAll(r.files.stackDir(name)); err != nil {
				r.logger.Error("forget a stack", "stack", name, "error", err)
			}
		}
	}
	r.removeSecretsOfOthers(desired)
}

// stop runs compose down for a stack that left this host, once: its
// Compose file goes with it.
func (r *stackRunner) stop(ctx context.Context, name string) error {
	file := r.files.stackFile(name)
	if _, err := os.Stat(file); errors.Is(err, os.ErrNotExist) {
		return nil // stopped before
	}
	release := r.supervisor.Hold(name)
	err := r.docker.ComposeDown(ctx, name, file)
	release()
	if err != nil {
		return err
	}
	r.mu.Lock()
	delete(r.applied, name)
	r.mu.Unlock()
	return os.Remove(file)
}

// removeVolumes removes the recorded copies of a stack's volumes that the
// state no longer places on this host in their generation, and tells how
// many copies are left.
func (r *stackRunner) removeVolumes(ctx context.Context, desired desiredState, stackName string) (int, error) {
	recorded, err := r.readRecord(stackName)
	if err != nil {
		return 0, err
	}
	var kept []storage.Volume
	var errs []error
	for _, volume := range recorded {
		if r.placedHere(desired, volume) {
			kept = append(kept, volume)
			continue
		}
		if err := r.storage.Remove(ctx, volume); err != nil {
			errs = append(errs, err)
			kept = append(kept, volume)
		}
	}
	if err := r.writeRecord(stackName, kept); err != nil {
		errs = append(errs, err)
	}
	return len(kept), errors.Join(errs...)
}

// placedHere tells whether the state has a copy of the volume on this host
// in the recorded generation.
func (r *stackRunner) placedHere(desired desiredState, volume storage.Volume) bool {
	current, ok := desired.volumes[volume.ID]
	if !ok || !slices.ContainsFunc(current.Placements, func(p state.Placement) bool { return p.Host == r.host.ID }) {
		return false
	}
	stackName, name, _ := strings.Cut(volume.ID, "/")
	return compose.VolumeName(stackName, name, current.Generation) == volume.LV
}
