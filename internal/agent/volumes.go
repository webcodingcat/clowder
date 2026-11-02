package agent

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/webcodingcat/clowder/internal/atomicfile"
	"github.com/webcodingcat/clowder/internal/clusterspec"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/state"
	"github.com/webcodingcat/clowder/internal/storage"
)

// trimInterval is how often the volumes give free blocks back.
const trimInterval = 7 * 24 * time.Hour

// describe turns a volume of the state into this host's copy of it, as the
// storage driver takes it. The LV's name carries the generation, so a
// restored volume is a new LV next to the old one; the DRBD resource has the
// LV's name.
func (r *stackRunner) describe(desired desiredState, id string) (storage.Volume, error) {
	volume, ok := desired.volumes[id]
	if !ok {
		return storage.Volume{}, fmt.Errorf("volume %s is not placed yet", id)
	}
	index := slices.IndexFunc(volume.Copies, func(c state.VolumeCopy) bool { return c.Host == r.host.ID })
	if index < 0 {
		return storage.Volume{}, fmt.Errorf("volume %s has no copy on this host", id)
	}
	lv := names.VolumeName(id, volume.DataGeneration)
	pool, err := poolOf(desired.config, id, volume.Copies[index].Class)
	if err != nil {
		return storage.Volume{}, err
	}
	described := storage.Volume{ID: id, LV: lv, Pool: pool, SizeBytes: int64(volume.Spec.Size)}
	if volume.Minor == nil {
		return described, nil // replicas 0: a plain LV
	}
	resource := &storage.Resource{Name: lv, Minor: *volume.Minor, ResyncRate: desired.config.Replication.ResyncRate}
	for _, volumeCopy := range volume.Copies {
		node := storage.Node{Host: desired.hostNames[volumeCopy.Host], ID: volumeCopy.Host}
		if !volumeCopy.Tiebreaker {
			peerPool, err := poolOf(desired.config, id, volumeCopy.Class)
			if err != nil {
				return storage.Volume{}, err
			}
			// a pool is "<volume group>/<thin pool>"; the LV's device is in
			// the volume group
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
func poolOf(config clusterspec.Config, volumeID, class string) (string, error) {
	storageClass, ok := config.StorageClasses[class]
	if !ok || storageClass.Pool == "" {
		return "", fmt.Errorf("volume %s: cluster.yaml has no storage class %s with a pool", volumeID, class)
	}
	return storageClass.Pool, nil
}

// removeVolumes removes the recorded copies on this host that the state
// retired here, and forgets them in the state.
func (r *stackRunner) removeVolumes(ctx context.Context, desired desiredState) {
	recorded, err := r.recordedCopies()
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

// removeCopies removes the copies of one volume on this host that the
// state has retired here and no longer places here in their data
// generation.
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
			errs = append(errs, fmt.Errorf("remove copy %s: %w", volume.LV, err))
			kept = append(kept, volume)
			continue
		}
		// Remove picks up where it left, so a copy whose forgetting failed
		// stays recorded and is tried again at the next pass
		if err := commitAsHost(ctx, r.propose, r.host.ID, state.KindForgetCopy, state.ForgetCopy{Copy: retired}); err != nil {
			errs = append(errs, fmt.Errorf("forget copy %s: %w", volume.LV, err))
			kept = append(kept, volume)
		}
	}
	if err := r.writeRecord(id, kept); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// placedHere tells whether the state has a copy of the volume on this host
// in the recorded data generation.
func (r *stackRunner) placedHere(desired desiredState, volume storage.Volume) bool {
	current, ok := desired.volumes[volume.ID]
	if !ok || !slices.ContainsFunc(current.Copies, func(c state.VolumeCopy) bool { return c.Host == r.host.ID }) {
		return false
	}
	return names.VolumeName(volume.ID, current.DataGeneration) == volume.LV
}

// retiredHere finds the retired copy in the state that matches the copy
// recorded on this host.
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
	if r.unexplained[volume.LV] {
		return
	}
	r.unexplained[volume.LV] = true
	r.logger.Warn("keeping a volume copy the cluster neither places nor retires here; remove it by hand if it is not needed",
		"volume", volume.ID, "lv", volume.LV)
}

// trim gives the free blocks of this host's volumes back to their pools
// once every trimInterval, until ctx ends. The time of the last trim is a
// file, so restarts of the agent do not put it off. It runs beside the
// runner's loop, so it uses none of the runner's loop-only fields.
func (r *stackRunner) trim(ctx context.Context) {
	for {
		info, err := os.Stat(r.files.lastTrim())
		if err != nil || time.Since(info.ModTime()) >= trimInterval {
			r.trimNow(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Hour):
		}
	}
}

// trimNow trims every recorded copy and remembers when.
func (r *stackRunner) trimNow(ctx context.Context) {
	recorded, err := r.recordedCopies()
	if err != nil {
		r.logger.Error("trim the volumes", "error", err)
		return
	}
	var volumes []storage.Volume
	for _, id := range slices.Sorted(maps.Keys(recorded)) {
		volumes = append(volumes, recorded[id]...)
	}
	if err := r.storage.Trim(ctx, volumes); err != nil {
		r.logger.Error("trim the volumes", "error", err)
	}
	// written after a failure too: the next try is in a week, not in an hour
	if err := atomicfile.Write(r.files.lastTrim(), nil, 0o600); err != nil {
		r.logger.Error("remember the trim", "error", err)
	}
}
