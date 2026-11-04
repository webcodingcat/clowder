// Package volumes is the agent's volume reconciler: it keeps every copy of
// a volume that the state places on this host, whether or not a stack here
// uses it. A copy that a stack here names is primary and mounted; any other
// is unmounted and secondary, so a stack on another host can take the
// volume; a retired one is removed. The stack runner asks it whether a
// copy is mounted (Mounted) and waits until it is.
//
// A pass only reads: what the state wants (desired.go), what the host has
// (storage.Driver.Observe), and which steps lead from one to the other
// (plan.go). The steps run in a job per volume, in a goroutine of its own
// that the pass does not wait for: mkfs, a mount or an unmount write to the
// volume and hang while DRBD suspends I/O, and that must hold up neither
// the other volumes nor the agent. The copies' records are in records.go,
// the weekly trim in trim.go.
package volumes

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"

	"github.com/webcodingcat/clowder/internal/agent/loop"
	"github.com/webcodingcat/clowder/internal/state"
	"github.com/webcodingcat/clowder/internal/storage"
)

// Storage is what the reconciler needs from the storage driver;
// storage.Driver is the real one.
type Storage interface {
	Observe(ctx context.Context, volumes []storage.Volume) (map[string]storage.Observed, error)
	Ensure(ctx context.Context, volume storage.Volume) error
	Promote(ctx context.Context, volume storage.Volume, force bool) error
	Mount(ctx context.Context, volume storage.Volume, format bool) error
	Unmount(ctx context.Context, volume storage.Volume) error
	Demote(ctx context.Context, volume storage.Volume) error
	Remove(ctx context.Context, volume storage.Volume) error
	Trim(ctx context.Context, volume storage.Volume) error
	ResolveSplitBrain(ctx context.Context, volume storage.Volume, discard bool) error
	MountPoint(volume storage.Volume) string
}

// Files are where the reconciler keeps what it knows of the copies; the
// agent's files say where each is.
type Files struct {
	// RecordsDir has a file per volume with its copies on this host
	RecordsDir string
	// LastTrim is the file whose time is the last trim
	LastTrim string
}

// Parts are what the reconciler works with, from the agent.
type Parts struct {
	Files   Files
	Host    state.HostID
	Cluster loop.Cluster
	Storage Storage
	// Held returns the LVs that stacks' containers on this host may hold
	// (stacks.Files.HeldLVs): such a copy stays as it is
	Held func() (map[string]bool, error)
	// Signal starts the reconciler's next pass; a job fires it when done
	Signal *loop.Signal
	// StacksReady starts a pass of the stack runner: a copy it waits for
	// is mounted
	StacksReady func()
	// Propose commits a command (a controller.Propose, spelled out as in
	// loop.CommitAsHost)
	Propose func(context.Context, state.Command) (uint64, error)
	Logger  *slog.Logger
}

// Reconciler keeps this host's volume copies. Only New sets its parts, so
// nothing changes them while its loops run.
type Reconciler struct {
	files       Files
	host        state.HostID
	cluster     loop.Cluster
	storage     Storage
	held        func() (map[string]bool, error)
	signal      *loop.Signal
	stacksReady func()
	propose     func(context.Context, state.Command) (uint64, error)
	logger      *slog.Logger

	// jobs counts the running jobs, so Run returns only after them
	jobs sync.WaitGroup

	// mu guards what the pass, the jobs, Mounted and Trim share.
	mu sync.Mutex
	// busy are the volumes, by ID, that have a job running: one job per
	// volume at a time, so its steps and its record never interleave
	busy map[string]bool
	// copies are what the last pass found of each copy, by LV
	copies map[string]copyStatus
	// mounted are the copies the last pass saw mounted, for the trim
	mounted []storage.Volume
	// volumes are the copies here as the last pass described them, by
	// volume ID, and seen what it found of each (Seen)
	volumes map[string]storage.Volume
	seen    map[string]storage.Observed
	// reporting are the copies, by LV, whose set-copy-synced is on its way
	reporting map[string]bool

	// unexplained are the LVs of copies kept without a reason, warned
	// about once; only the pass uses it
	unexplained map[string]bool
}

// copyStatus is what the stack runner learns of a copy (Mounted).
type copyStatus struct {
	dir   string // the mount point
	ready bool   // primary and mounted, as a stack here needs it
	// released: unmounted and secondary, so a stack elsewhere can take it
	released bool
	err      error // why the copy cannot get there
}

// New makes the reconciler; it starts nothing.
func New(parts Parts) *Reconciler {
	return &Reconciler{
		files: parts.Files, host: parts.Host, cluster: parts.Cluster, storage: parts.Storage, held: parts.Held,
		signal: parts.Signal, stacksReady: parts.StacksReady, propose: parts.Propose, logger: parts.Logger,
		busy: map[string]bool{}, copies: map[string]copyStatus{}, reporting: map[string]bool{},
		unexplained: map[string]bool{},
	}
}

// Mounted tells the stack runner about a copy, by its LV: its mount point
// and ready once it is primary and mounted; an error if it cannot get
// there. Not ready without an error means it is on its way: the stack
// waits.
func (r *Reconciler) Mounted(lv string) (dir string, ready bool, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	status := r.copies[lv]
	return status.dir, status.ready, status.err
}

// Run runs passes until ctx ends (loop.Run), then waits for the jobs: a job
// left hanging on a suspended volume holds up the agent's stop only as long
// as the agent waits for its loops.
func (r *Reconciler) Run(ctx context.Context) {
	loop.Run(ctx, r.cluster, r.signal.Wake(), func() { r.pass(ctx) })
	r.jobs.Wait()
}

// pass compares the copies the state wants here with what the host has,
// and starts a job for each volume whose copy needs a step.
func (r *Reconciler) pass(ctx context.Context) {
	copied, err := r.cluster.SharedCopy()
	if err != nil {
		r.logger.Error("read the state", "error", err)
		return
	}
	held, err := r.held()
	// not knowing what the stacks hold, any copy may be in use
	holdAll := err != nil
	if holdAll {
		r.logger.Error("read the volumes the stacks hold", "error", err)
	}
	wanted := r.desired(copied, held, holdAll)
	var volumes []storage.Volume
	for _, want := range wanted {
		if want.err == nil {
			volumes = append(volumes, want.volume)
		}
	}
	observed, err := r.storage.Observe(ctx, volumes)
	if err != nil {
		r.logger.Error("look at the volume copies", "error", err)
		return
	}
	var mounted []storage.Volume
	placed := map[string]bool{}
	described := map[string]storage.Volume{}
	seenNow := map[string]storage.Observed{}
	for _, want := range wanted {
		seen := observed[want.volume.LV]
		if seen.Mounted && !seen.OtherDevice {
			mounted = append(mounted, want.volume)
		}
		placed[want.volume.LV] = true
		if want.err == nil {
			described[want.volume.ID], seenNow[want.volume.ID] = want.volume, seen
		}
		r.reportSynced(ctx, want, seen)
		r.reconcile(ctx, want, seen)
	}
	r.mu.Lock()
	r.mounted, r.volumes, r.seen = mounted, described, seenNow
	// a copy the state no longer places here is no stack's to use
	maps.DeleteFunc(r.copies, func(lv string, _ copyStatus) bool { return !placed[lv] })
	r.mu.Unlock()
	r.removeRetired(ctx, copied, held, holdAll)
}

// reconcile starts the steps a copy needs, unless its volume has a job
// running already; with none needed, it tells what the copy is.
func (r *Reconciler) reconcile(ctx context.Context, want wantedCopy, seen storage.Observed) {
	volume := want.volume
	if r.isBusy(volume.ID) {
		return
	}
	if want.err != nil {
		r.setStatus(volume, copyStatus{err: want.err})
		return
	}
	steps, err := plan(copyFacts{
		target: want.target, plain: volume.Resource == nil, diskless: volume.Diskless, initialized: want.initialized, seen: seen,
	})
	switch {
	case err != nil:
		r.setStatus(volume, copyStatus{err: fmt.Errorf("volume %s: %w", volume.ID, err)})
	case len(steps) == 0:
		r.setStatus(volume, copyStatus{
			dir: r.storage.MountPoint(volume), ready: want.target == targetMounted,
			released: want.target == targetSecondary,
		})
	default:
		r.start(ctx, volume.ID, func() error { return r.runSteps(ctx, want, steps) })
	}
}

// isBusy tells whether a volume has a job running.
func (r *Reconciler) isBusy(volumeID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.busy[volumeID]
}

// claim marks a volume busy, unless it is already; the trim claims one too,
// so no job unmounts a copy while fstrim runs on it.
func (r *Reconciler) claim(volumeID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.busy[volumeID] {
		return false
	}
	r.busy[volumeID] = true
	return true
}

// release marks a volume free again. After a job that worked, the next pass
// comes at once and takes the copy on; after one that failed, it comes with
// the next change of the state or loop.Interval: tried again at once, a step
// that keeps failing would spin.
func (r *Reconciler) release(ctx context.Context, volumeID string, failed bool) {
	r.mu.Lock()
	delete(r.busy, volumeID)
	r.mu.Unlock()
	if ctx.Err() == nil && !failed {
		r.signal.Now()
	}
}

// start runs a job for a volume in a goroutine of its own; the pass
// checked that the volume is not busy, and only the pass starts jobs.
func (r *Reconciler) start(ctx context.Context, volumeID string, job func() error) {
	if !r.claim(volumeID) {
		return // the trim took it meanwhile; the next pass tries again
	}
	r.jobs.Go(func() {
		err := job()
		r.release(ctx, volumeID, err != nil)
	})
}

// setStatus records what a copy is. An error is logged only when it is
// new, so a copy stuck on the same error does not fill the log at every
// pass. When a copy becomes ready, the stack runner starts a pass.
func (r *Reconciler) setStatus(volume storage.Volume, status copyStatus) {
	r.mu.Lock()
	old := r.copies[volume.LV]
	r.copies[volume.LV] = status
	r.mu.Unlock()
	if status.err != nil && (old.err == nil || old.err.Error() != status.err.Error()) {
		r.logger.Error("a volume copy cannot be brought to what the state says",
			"volume", volume.ID, "lv", volume.LV, "error", status.err)
	}
	if status.ready && !old.ready {
		r.stacksReady()
	}
}

// runSteps runs a copy's steps in order and stops at the first that fails;
// the next pass plans afresh.
func (r *Reconciler) runSteps(ctx context.Context, want wantedCopy, steps []step) error {
	volume := want.volume
	if want.target != targetMounted {
		// the job gives the copy up: no stack may take it meanwhile
		r.setStatus(volume, copyStatus{})
	}
	for _, step := range steps {
		if err := r.runStep(ctx, want, step); err != nil {
			if ctx.Err() == nil {
				r.setStatus(volume, copyStatus{err: fmt.Errorf("volume %s: %s: %w", volume.ID, step, err)})
			}
			return err
		}
	}
	return nil
}

// runStep runs one step on a copy.
func (r *Reconciler) runStep(ctx context.Context, want wantedCopy, step step) error {
	volume := want.volume
	switch step {
	case stepEnsure:
		// recorded before it exists, so a copy made by a crashed job is
		// still found and removed later
		if err := r.recordCopy(volume); err != nil {
			return err
		}
		return r.storage.Ensure(ctx, volume)
	case stepPromote, stepForcePromote:
		return r.storage.Promote(ctx, volume, step == stepForcePromote)
	case stepMount, stepFormatAndMount:
		return r.storage.Mount(ctx, volume, step == stepFormatAndMount)
	case stepSetInitialized:
		return loop.CommitAsHost(ctx, r.propose, r.host, state.KindSetVolumeInitialized, state.SetVolumeInitialized{
			Volume: volume.ID, DataGeneration: want.dataGeneration,
		})
	case stepUnmount:
		return r.storage.Unmount(ctx, volume)
	case stepDemote:
		return r.storage.Demote(ctx, volume)
	default:
		return fmt.Errorf("unknown step %q", step)
	}
}

// removeRetired removes the recorded copies on this host that the state
// retired here, each in a job of its volume, and forgets them in the state.
// A copy a stack may still hold waits until it stopped.
func (r *Reconciler) removeRetired(ctx context.Context, copied *state.State, held map[string]bool, holdAll bool) {
	recorded, err := r.recordedCopies()
	if err != nil {
		r.logger.Error("read the volume copies of this host", "error", err)
		return
	}
	for _, id := range slices.Sorted(maps.Keys(recorded)) {
		for _, volume := range recorded[id] {
			if r.placedHere(copied, volume) || held[volume.LV] || holdAll || r.isBusy(id) {
				continue
			}
			retired, ok := r.retiredHere(copied, volume)
			if !ok {
				// neither placed nor retired: a state that lost it by
				// mistake must not cost the data
				r.warnUnexplained(volume)
				continue
			}
			r.start(ctx, id, func() error { return r.removeCopy(ctx, volume, retired) })
		}
	}
}

// removeCopy removes a retired copy, forgets it in the state and then in
// its record. Remove picks up where it left, so a copy whose forgetting
// failed stays recorded and is tried again at the next pass.
func (r *Reconciler) removeCopy(ctx context.Context, volume storage.Volume, retired state.RetiredCopy) error {
	err := r.storage.Remove(ctx, volume)
	if err == nil {
		err = loop.CommitAsHost(ctx, r.propose, r.host, state.KindForgetCopy, state.ForgetCopy{Copy: retired})
	}
	if err == nil {
		err = r.forgetCopy(volume)
	}
	switch {
	case errors.Is(err, storage.ErrNotYet):
		// a primary peer still has it; the next pass tries again
		r.logger.Debug("a retired volume copy waits", "volume", volume.ID, "lv", volume.LV, "reason", err)
	case err != nil:
		r.logger.Error("remove a retired volume copy", "volume", volume.ID, "lv", volume.LV, "error", err)
	}
	return err
}

// warnUnexplained says once per agent run that a copy is kept which the
// state neither places nor retires here.
func (r *Reconciler) warnUnexplained(volume storage.Volume) {
	if r.unexplained[volume.LV] {
		return
	}
	r.unexplained[volume.LV] = true
	r.logger.Warn("keeping a volume copy the cluster neither places nor retires here; remove it by hand if it is not needed",
		"volume", volume.ID, "lv", volume.LV)
}

// reportSynced tells the state when a copy with a disk became synced or
// stopped being so (set-copy-synced): a move waits for it, and users see
// it. A resource that is down says nothing either way, so a restart of
// the agent writes nothing. A failed commit is tried again at the next
// pass, which compares with the state again.
func (r *Reconciler) reportSynced(ctx context.Context, want wantedCopy, seen storage.Observed) {
	volume := want.volume
	if want.err != nil || volume.Diskless || (volume.Resource != nil && seen.Disk == "") {
		return
	}
	synced := seen.Synced(volume)
	if synced == want.synced {
		return
	}
	r.mu.Lock()
	reporting := r.reporting[volume.LV]
	r.reporting[volume.LV] = true
	r.mu.Unlock()
	if reporting {
		return // the last report is on its way
	}
	// in a goroutine of its own: without Raft's quorum a commit waits for
	// its timeout, and the pass must not
	r.jobs.Go(func() {
		defer func() {
			r.mu.Lock()
			delete(r.reporting, volume.LV)
			r.mu.Unlock()
		}()
		err := loop.CommitAsHost(ctx, r.propose, r.host, state.KindSetCopySynced, state.SetCopySynced{
			Volume: volume.ID, DataGeneration: want.dataGeneration, Synced: synced,
		})
		if err != nil && ctx.Err() == nil {
			r.logger.Error("tell whether a volume copy is synced", "volume", volume.ID, "synced", synced, "error", err)
		}
	})
}

// Released tells whether this host's copy of a volume, by its LV, is
// unmounted and secondary as the state wants it, so a stack on another
// host can take the volume.
func (r *Reconciler) Released(lv string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.copies[lv].released
}

// Progress is what each copy on this host, by volume ID, differed by from
// a peer at the last pass: while it resyncs, what is left of the resync,
// in bytes. It is the part of Seen that the API passes on to the leader's
// moves; the API is portable and does not know storage's types.
func (r *Reconciler) Progress() map[string]int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	progress := map[string]int64{}
	for id, seen := range r.seen {
		progress[id] = seen.OutOfSyncBytes
	}
	return progress
}

// Seen is what the last pass found of each copy on this host, by volume
// ID: its DRBD role, disk state, quorum and split brain, for the host's
// report and /metrics.
func (r *Reconciler) Seen() map[string]storage.Observed {
	r.mu.Lock()
	defer r.mu.Unlock()
	return maps.Clone(r.seen)
}

// ResolveSplitBrain connects this host's copy of a volume again to the
// peers it stands alone from (storage.Driver.ResolveSplitBrain); with
// discard, the copy takes their data; connected is false when no peer was
// StandAlone. It runs while no job holds the
// volume, and holds it meanwhile: the command touches no data on the copy
// to keep, and a copy that discards is secondary, so neither waits on a
// write.
func (r *Reconciler) ResolveSplitBrain(ctx context.Context, volumeID string, discard bool) (connected bool, err error) {
	r.mu.Lock()
	volume, ok := r.volumes[volumeID]
	r.mu.Unlock()
	if !ok {
		return false, fmt.Errorf("this host has no copy of volume %s", volumeID)
	}
	if !r.claim(volumeID) {
		return false, fmt.Errorf("volume %s: a step runs on the copy here; try again", volumeID)
	}
	err = r.storage.ResolveSplitBrain(ctx, volume, discard)
	if errors.Is(err, storage.ErrNoSplitBrain) {
		// nothing to do here, which is no failure
		r.release(ctx, volumeID, false)
		return false, nil
	}
	r.release(ctx, volumeID, err != nil)
	return err == nil, err
}
