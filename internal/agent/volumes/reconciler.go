// Package volumes is the agent's volume reconciler. It keeps every copy of
// a volume that the state places on this host, whether or not a stack on
// this host uses it. A copy that a local stack names is primary and
// mounted. Any other copy is unmounted and secondary, so that a stack on
// another host can take the volume. A retired copy is removed. The stack
// runner asks the reconciler whether a copy is mounted (Mounted) and waits
// until it is.
//
// A pass only reads: what the state wants (desired.go), what the host has
// (storage.Driver.Observe), and which steps lead from one to the other
// (plan.go). The steps run in one job per volume, in a separate goroutine
// that the pass does not wait for. The reason: mkfs, a mount or an unmount
// writes to the volume and hangs while DRBD suspends I/O, and that must
// block neither the other volumes nor the agent. The records of the copies
// are in records.go, the weekly trim is in trim.go.
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

// Files are the files where the reconciler keeps what it knows about the
// copies. The agent's files give their paths.
type Files struct {
	// RecordsDir has one file per volume that lists its copies on this host
	RecordsDir string
	// LastTrim is the file whose modification time is the time of the last trim
	LastTrim string
}

// Parts are what the agent gives the reconciler to work with.
type Parts struct {
	Files   Files
	Host    state.HostID
	Cluster loop.Cluster
	Storage Storage
	// Held returns the LVs that the containers of stacks on this host may
	// hold (stacks.Files.HeldLVs); the reconciler leaves such a copy as it is
	Held func() (map[string]bool, error)
	// Signal starts the reconciler's next pass; a job fires it when it is done
	Signal *loop.Signal
	// StacksReady starts a pass of the stack runner when a copy that the
	// runner waits for is mounted
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

	// jobs counts the running jobs, so that Run returns only after they end
	jobs sync.WaitGroup

	// mu guards the fields that the pass, the jobs, Mounted and Trim share.
	mu sync.Mutex
	// busy holds the IDs of the volumes that have a job running. Only one
	// job runs per volume at a time, so its steps and its record never
	// interleave with another job's
	busy map[string]bool
	// copies holds what the last pass found about each copy, by LV
	copies map[string]copyStatus
	// mounted holds the copies that the last pass saw mounted, for the trim
	mounted []storage.Volume
	// volumes holds the copies on this host as the last pass described
	// them, by volume ID; seen holds what the pass observed about each
	// (Seen)
	volumes map[string]storage.Volume
	seen    map[string]storage.Observed
	// reporting holds the LVs of the copies whose set-copy-synced is on its way
	reporting map[string]bool

	// unexplained holds the LVs of copies that are kept without a reason
	// and were warned about once; only the pass uses it
	unexplained map[string]bool
}

// copyStatus is what the stack runner learns about a copy (Mounted).
type copyStatus struct {
	dir   string // the mount point
	ready bool   // primary and mounted, as a local stack needs it
	// released: unmounted and secondary, so a stack on another host can take it
	released bool
	err      error // why the copy cannot reach its target
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

// Mounted tells the stack runner about a copy, by its LV. It returns the
// mount point, and ready once the copy is primary and mounted. It returns
// an error if the copy cannot get there. Not ready and no error means that
// the copy is on its way, and the stack waits.
func (r *Reconciler) Mounted(lv string) (dir string, ready bool, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	status := r.copies[lv]
	return status.dir, status.ready, status.err
}

// Run runs passes until ctx ends (loop.Run), then waits for the jobs. A job
// that hangs on a suspended volume delays the agent's stop only as long as
// the agent waits for its loops.
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
	// without knowing what the stacks hold, any copy may be in use
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
	// no stack may use a copy that the state no longer places here
	maps.DeleteFunc(r.copies, func(lv string, _ copyStatus) bool { return !placed[lv] })
	r.mu.Unlock()
	r.removeRetired(ctx, copied, held, holdAll)
}

// reconcile starts the steps that a copy needs, unless its volume already
// has a job running. If no step is needed, it records the copy's status.
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

// claim marks a volume busy, unless it is busy already. The trim claims
// volumes too, so that no job unmounts a copy while fstrim runs on it.
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
// starts at once and continues with the copy. After a job that failed, the
// next pass waits for the next change of the state or for loop.Interval: a
// step that keeps failing would spin if it were tried again at once.
func (r *Reconciler) release(ctx context.Context, volumeID string, failed bool) {
	r.mu.Lock()
	delete(r.busy, volumeID)
	r.mu.Unlock()
	if ctx.Err() == nil && !failed {
		r.signal.Now()
	}
}

// start runs a job for a volume in a separate goroutine. The pass has
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

// setStatus records a copy's status. It logs an error only when the error
// is new, so that a copy stuck on the same error does not fill the log at
// every pass. When a copy becomes ready, the stack runner starts a pass.
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

// runSteps runs a copy's steps in order and stops at the first one that
// fails. The next pass makes a new plan.
func (r *Reconciler) runSteps(ctx context.Context, want wantedCopy, steps []step) error {
	volume := want.volume
	if want.target != targetMounted {
		// the job gives up the copy, so no stack may take it in the meantime
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
		// recorded before it exists, so that a copy made by a crashed job
		// is still found and removed later
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
// A copy that a stack may still hold waits until the stack has stopped.
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
				// neither placed nor retired: if the state lost the copy
				// by mistake, the data must not be lost too
				r.warnUnexplained(volume)
				continue
			}
			r.start(ctx, id, func() error { return r.removeCopy(ctx, volume, retired) })
		}
	}
}

// removeCopy removes a retired copy, then forgets it in the state and then
// in its record. Remove continues where it stopped before, so a copy that
// could not be forgotten stays recorded, and the next pass tries again.
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

// warnUnexplained warns once per agent run that the agent keeps a copy that
// the state neither places nor retires here.
func (r *Reconciler) warnUnexplained(volume storage.Volume) {
	if r.unexplained[volume.LV] {
		return
	}
	r.unexplained[volume.LV] = true
	r.logger.Warn("keeping a volume copy the cluster neither places nor retires here; remove it by hand if it is not needed",
		"volume", volume.ID, "lv", volume.LV)
}

// reportSynced tells the state when a copy with a disk becomes synced or
// stops being synced (set-copy-synced). A move waits for this, and users
// see it. A resource that is down reports nothing either way, so a restart
// of the agent writes nothing. A failed commit is tried again at the next
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
	// in a separate goroutine: without Raft's quorum a commit waits for
	// its timeout, and the pass must not wait with it
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

// Progress returns, for each copy on this host by volume ID, how many bytes
// it differed from a peer at the last pass: while a copy resyncs, this is
// what is left of the resync. It is the part of Seen that the API passes on
// to the leader's moves; the API is portable and does not know storage's
// types.
func (r *Reconciler) Progress() map[string]int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	progress := map[string]int64{}
	for id, seen := range r.seen {
		progress[id] = seen.OutOfSyncBytes
	}
	return progress
}

// Seen returns what the last pass found about each copy on this host, by
// volume ID: its DRBD role, disk state, quorum and split brain. The host's
// report and /metrics use it.
func (r *Reconciler) Seen() map[string]storage.Observed {
	r.mu.Lock()
	defer r.mu.Unlock()
	return maps.Clone(r.seen)
}

// ResolveSplitBrain reconnects this host's copy of a volume to the peers
// it stands alone from (storage.Driver.ResolveSplitBrain). With discard,
// the copy takes their data. connected is false when no peer was
// StandAlone. It runs only while no job holds the volume, and it holds the
// volume while it runs. It never waits on a write: the command touches no
// data on the copy that keeps its data, and a copy that discards is
// secondary.
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
