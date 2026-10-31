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
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/webcodingcat/clowder/internal/atomicfile"
	"github.com/webcodingcat/clowder/internal/clusterdir"
	"github.com/webcodingcat/clowder/internal/compose"
	"github.com/webcodingcat/clowder/internal/controller"
	"github.com/webcodingcat/clowder/internal/docker"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/network"
	"github.com/webcodingcat/clowder/internal/state"
	"github.com/webcodingcat/clowder/internal/storage"
	"github.com/webcodingcat/clowder/internal/supervisor"
)

// Docker is what the agent needs from Docker; docker.Client is the real one.
type Docker interface {
	supervisor.Docker
	ComposeUp(ctx context.Context, stack string, files, leftOut []string) error
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

// stateReader is what the agent's loops need from the Raft member.
type stateReader interface {
	Read(use func(current *state.State))
	Changed() <-chan struct{}
}

// stackRunner runs the stacks the state places on this host.
type stackRunner struct {
	files   Files
	host    Host
	cluster stateReader
	docker  Docker
	storage Storage
	open    func(sealed state.Sealed) ([]byte, error)
	// digestKey keys the digests of secret files (secretDigestPurpose)
	digestKey []byte
	logger    *slog.Logger
	// supervisor needs the runner's rules first, so Run sets it afterwards
	supervisor *supervisor.Supervisor
	propose    controller.Propose

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
	// unexplained are the LVs of copies kept without a reason, warned
	// about once
	unexplained map[string]bool
}

// desiredState is what this host should run, from a copy of the state.
type desiredState struct {
	config clusterdir.Config
	// stacks are by their Compose project (Stack.Project), the name of all
	// the agent keeps per stack
	stacks     map[string]*state.Stack
	volumes    map[string]*state.Volume
	hostNames  map[state.HostID]string
	registries map[string]state.Sealed
	// retired are the copies the cluster gave up, on any host
	retired []state.RetiredCopy
}

// readDesired reads what the host self should run: a copy of the state,
// made under the lock Raft's Apply waits for, which a pass then works on
// outside it.
func readDesired(cluster stateReader, self state.HostID) (desiredState, error) {
	var copied *state.State
	var err error
	cluster.Read(func(current *state.State) { copied, err = current.Copy() })
	if err != nil {
		return desiredState{}, err
	}
	desired := desiredState{
		config: copied.Config.Spec, stacks: map[string]*state.Stack{}, volumes: copied.Volumes,
		hostNames: map[state.HostID]string{}, registries: copied.Registries, retired: copied.Retired,
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

// pass brings this host once to what the state says. The stacks that left
// stop first, before the others come up, so a stack that comes never
// starts beside one that is still going. (While a volume belongs to its
// stack, no volume passes between stacks; when volumes are resources of
// their own, a stack whose volume a stack that failed to stop still holds
// must wait as well.)
func (r *stackRunner) pass(ctx context.Context) {
	desired, err := readDesired(r.cluster, r.host.ID)
	if err != nil {
		r.logger.Error("read the state", "error", err)
		return
	}
	r.logIn(ctx, desired)
	r.stopLeavers(ctx, desired)
	for _, project := range slices.Sorted(maps.Keys(desired.stacks)) {
		if err := r.apply(ctx, desired, desired.stacks[project]); err != nil {
			r.logger.Error("run a stack", "stack", desired.stacks[project].ID(), "error", err)
		}
	}
	r.removeVolumes(ctx, desired)
	r.removeSecretsOfOthers(desired)
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
	project := stack.Project()
	volumes := map[string]storage.Volume{}
	mounted := map[string]compose.MountedVolume{}
	for name := range parsed.Volumes {
		volume, err := r.describe(desired, stack.VolumeID(name))
		if err != nil {
			return err
		}
		volumes[name] = volume
		mounted[name] = compose.MountedVolume{Dir: r.storage.MountPoint(volume), Name: volume.LV}
	}
	// Rendered before any volume is made, so a stack that cannot run leaves
	// nothing behind. The parse error is not needed: cluster.yaml's check
	// refuses a Floating IP that does not parse, and before the first apply
	// it is empty, the zero address, which Render refuses.
	floatingIP, _ := netip.ParseAddr(desired.config.Ingress.FloatingIP)
	rendered, err := compose.Render(parsed, compose.Target{
		Namespace: stack.Namespace, FloatingIP: floatingIP, NodeAddress: r.host.NodeAddress,
		Resolver: network.Resolver, Volumes: mounted, SecretsDir: r.files.secretFilesDir(project),
		SecretDigests: secrets.digests,
	})
	if err != nil {
		return err
	}
	// recorded before they exist, so a copy made by a crashed pass is still
	// found and removed later
	if err := r.record(slices.Collect(maps.Values(volumes))); err != nil {
		return err
	}
	for _, volume := range volumes {
		if err := r.storage.Ensure(ctx, volume); err != nil {
			return err
		}
	}
	if err := r.up(ctx, project, rendered, secrets, stack.PushedOut); err != nil {
		return err
	}
	if stack.Status == state.StackRunning {
		return nil
	}
	return commitAsHost(ctx, r.propose, r.host.ID, state.KindSetStackStatus, state.SetStackStatus{
		ID: stack.ID(), Status: state.StackRunning, Host: stack.Host, Generation: stack.Generation,
	})
}

// up writes a stack's Compose file and secrets and runs compose up without
// the pushed-out services, unless all of it came up before. A failed
// compose up is tried again at the next pass.
func (r *stackRunner) up(ctx context.Context, project string, rendered []byte, secrets stackSecrets,
	pushedOut []string,
) error {
	hash := upHash(rendered, secrets, pushedOut)
	r.mu.Lock()
	applied, ok := r.applied[project]
	r.mu.Unlock()
	if ok && applied == hash {
		return nil
	}
	file := r.files.stackFile(project)
	if err := atomicfile.Write(file, rendered, 0o600); err != nil {
		return err
	}
	if err := r.writeSecrets(project, secrets); err != nil {
		return err
	}
	files := []string{file}
	if secrets.env != nil {
		files = append(files, r.files.secretEnvFile(project))
	}
	if err := composeUpHeld(ctx, r.docker, r.supervisor, project, files, pushedOut); err != nil {
		return err
	}
	r.mu.Lock()
	r.applied[project] = hash
	r.mu.Unlock()
	return r.removeStaleSecrets(project, secrets)
}

// composeUpHeld runs compose up for a project while the supervisor holds
// it: containers that compose up stops or recreates do not crash.
func composeUpHeld(ctx context.Context, dockerAPI Docker, holder *supervisor.Supervisor, project string,
	files, leftOut []string,
) error {
	release := holder.Hold(project)
	defer release()
	return dockerAPI.ComposeUp(ctx, project, files, leftOut)
}

// upHash covers everything compose up reads: the rendered file, the secret
// variables, the secret files and the services left out (the state keeps
// them sorted).
func upHash(rendered []byte, secrets stackSecrets, pushedOut []string) [32]byte {
	hash := sha256.New()
	writeParts(hash, rendered, secrets.env)
	for _, name := range slices.Sorted(maps.Keys(secrets.files)) {
		writeParts(hash, []byte(name), secrets.files[name])
	}
	for _, service := range pushedOut {
		writeParts(hash, []byte("pushed out"), []byte(service))
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
	lv := names.VolumeName(id, volume.DataGeneration)
	pool, err := poolOf(desired.config, id, volume.Placements[index].Class)
	if err != nil {
		return storage.Volume{}, err
	}
	described := storage.Volume{ID: id, LV: lv, Pool: pool, SizeBytes: int64(volume.Spec.Size)}
	if volume.Minor == nil {
		return described, nil // replicas 0: a plain LV
	}
	resource := &storage.Resource{Name: lv, Minor: *volume.Minor, ResyncRate: desired.config.Replication.ResyncRate}
	for _, placement := range volume.Placements {
		node := storage.Node{Host: desired.hostNames[placement.Host], ID: placement.Host}
		if !placement.Tiebreaker {
			peerPool, err := poolOf(desired.config, id, placement.Class)
			if err != nil {
				return storage.Volume{}, err
			}
			group, _, _ := strings.Cut(peerPool, "/")
			node.Disk = "/dev/" + group + "/" + lv
		}
		resource.Nodes = append(resource.Nodes, node)
	}
	described.Resource = resource
	return described, nil
}

// poolOf is the thin pool of a copy's storage class. cluster apply refuses
// a cluster.yaml without a class a volume uses, so a missing one is a bug
// to say clearly, not an empty pool for lvcreate.
func poolOf(config clusterdir.Config, volumeID, class string) (string, error) {
	storageClass, ok := config.StorageClasses[class]
	if !ok || storageClass.Pool == "" {
		return "", fmt.Errorf("volume %s: cluster.yaml has no storage class %s with a pool", volumeID, class)
	}
	return storageClass.Pool, nil
}

// stopLeavers stops the stacks that left this host. The files tell how far
// a leaver got, so a crash at any point is picked up: its compose.yaml
// exists until compose down worked (stop), then its directory goes, and its
// secrets go once compose.yaml is gone (removeSecretsOfOthers). Its volume
// copies have records of their own (removeVolumes).
func (r *stackRunner) stopLeavers(ctx context.Context, desired desiredState) {
	entries, err := os.ReadDir(r.files.stacksDir())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		r.logger.Error("list the stacks of this host", "error", err)
		return
	}
	for _, entry := range entries {
		project := entry.Name()
		if _, staying := desired.stacks[project]; staying {
			continue
		}
		if err := r.stop(ctx, project); err != nil {
			r.logger.Error("stop a stack that left this host", "project", project, "error", err)
			continue
		}
		if err := os.RemoveAll(r.files.stackDir(project)); err != nil {
			r.logger.Error("forget a stack", "project", project, "error", err)
		}
	}
}

// stop runs compose down for a stack that left this host, once: its
// Compose file goes with it.
func (r *stackRunner) stop(ctx context.Context, project string) error {
	file := r.files.stackFile(project)
	if _, err := os.Stat(file); errors.Is(err, os.ErrNotExist) {
		return nil // stopped before
	}
	release := r.supervisor.Hold(project)
	err := r.docker.ComposeDown(ctx, project, file)
	release()
	if err != nil {
		return err
	}
	r.mu.Lock()
	delete(r.applied, project)
	r.mu.Unlock()
	return os.Remove(file)
}

// removeVolumes removes the recorded copies on this host that the state
// retired here, and forgets them in the state.
func (r *stackRunner) removeVolumes(ctx context.Context, desired desiredState) {
	recorded, err := r.recordedVolumes()
	if err != nil {
		r.logger.Error("read the volume copies of this host", "error", err)
		return
	}
	for _, id := range slices.Sorted(maps.Keys(recorded)) {
		if err := r.removeCopies(ctx, desired, id, recorded[id]); err != nil {
			r.logger.Error("remove the copies of a volume", "volume", id, "error", err)
		}
	}
}

// removeCopies removes those copies of one volume that the state no longer
// places on this host in their generation and retired here.
func (r *stackRunner) removeCopies(ctx context.Context, desired desiredState, id string, recorded []storage.Volume) error {
	var kept []storage.Volume
	var errs []error
	for _, volume := range recorded {
		if r.placedHere(desired, volume) {
			kept = append(kept, volume)
			continue
		}
		retired, ok := r.retiredHere(desired, volume)
		if !ok {
			// neither placed nor retired: a state that lost it by mistake
			// must not cost the data
			r.warnUnexplained(volume)
			kept = append(kept, volume)
			continue
		}
		if err := r.storage.Remove(ctx, volume); err != nil {
			errs = append(errs, err)
			kept = append(kept, volume)
			continue
		}
		// Remove picks up where it left, so a copy whose forgetting failed
		// stays recorded and is tried again at the next pass
		if err := commitAsHost(ctx, r.propose, r.host.ID, state.KindForgetCopy, state.ForgetCopy{Copy: retired}); err != nil {
			errs = append(errs, err)
			kept = append(kept, volume)
		}
	}
	if err := r.writeRecord(id, kept); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// retiredHere finds the retired copy that the recorded one on this host is.
func (r *stackRunner) retiredHere(desired desiredState, volume storage.Volume) (state.RetiredCopy, bool) {
	for _, retired := range desired.retired {
		if retired.Host == r.host.ID && retired.Volume == volume.ID &&
			names.VolumeName(volume.ID, retired.DataGeneration) == volume.LV {
			return retired, true
		}
	}
	return state.RetiredCopy{}, false
}

// warnUnexplained says once per agent run that a copy is kept which the
// state neither places nor retires here.
func (r *stackRunner) warnUnexplained(volume storage.Volume) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.unexplained[volume.LV] {
		return
	}
	r.unexplained[volume.LV] = true
	r.logger.Warn("keeping a volume copy the cluster neither places nor retires here; remove it by hand if it is not needed",
		"volume", volume.ID, "lv", volume.LV)
}

// placedHere tells whether the state has a copy of the volume on this host
// in the recorded data generation.
func (r *stackRunner) placedHere(desired desiredState, volume storage.Volume) bool {
	current, ok := desired.volumes[volume.ID]
	if !ok || !slices.ContainsFunc(current.Placements, func(p state.Placement) bool { return p.Host == r.host.ID }) {
		return false
	}
	return names.VolumeName(volume.ID, current.DataGeneration) == volume.LV
}
