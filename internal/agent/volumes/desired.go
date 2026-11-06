package volumes

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/webcodingcat/clowder/internal/clusterspec"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/state"
	"github.com/webcodingcat/clowder/internal/storage"
)

// wantedCopy is a copy that the state places on this host, and its target.
type wantedCopy struct {
	volume         storage.Volume
	dataGeneration int64
	target         target
	initialized    bool
	// synced is what the state says about the copy (VolumeCopy.Synced)
	synced bool
	// err says why the copy cannot be described (its class has no pool)
	err error
}

// desired returns the copies that the state places on this host, sorted by
// volume ID. A copy that is named by a stack placed on this host is
// mounted, unless the stack stops for a move (State.StopsForMove). A copy that a
// local stack may still hold (held, or any copy while holdAll) is kept as
// it is. Any other copy is secondary.
func (r *Reconciler) desired(copied *state.State, held map[string]bool, holdAll bool) []wantedCopy {
	named := map[string]bool{}
	for _, stack := range copied.Stacks {
		if stack.Host != nil && *stack.Host == r.host && !copied.StopsForMove(stack.ID()) {
			for _, name := range stack.Spec.VolumeNames() {
				named[names.ID(stack.Namespace, name)] = true
			}
		}
	}
	var wanted []wantedCopy
	for _, id := range slices.Sorted(maps.Keys(copied.Volumes)) {
		volume := copied.Volumes[id]
		index := slices.IndexFunc(volume.Copies, func(c state.VolumeCopy) bool { return c.Host == r.host })
		if index < 0 {
			continue
		}
		described, err := r.describe(copied, volume)
		want := wantedCopy{
			volume: described, dataGeneration: volume.DataGeneration, initialized: volume.Initialized,
			synced: volume.Copies[index].Synced, err: err,
		}
		switch {
		case named[id]:
			want.target = targetMounted
		case held[described.LV] || holdAll:
			want.target = targetKept
		default:
			want.target = targetSecondary
		}
		wanted = append(wanted, want)
	}
	return wanted
}

// describe turns a volume of the state into this host's copy of it, in the
// form that the storage driver expects. The LV's name contains the generation, so
// a restored volume is a new LV next to the old one. The DRBD resource has
// the LV's name. Even a copy that cannot be described has its ID and LV, so
// that the error reaches the stack that waits for it. The caller (desired)
// makes sure that the volume has a copy on this host.
func (r *Reconciler) describe(copied *state.State, volume *state.Volume) (storage.Volume, error) {
	id := volume.ID()
	lv := names.VolumeName(id, volume.DataGeneration)
	described := storage.Volume{ID: id, LV: lv, SizeBytes: int64(volume.Spec.Size)}
	config := copied.Config.Spec
	index := slices.IndexFunc(volume.Copies, func(c state.VolumeCopy) bool { return c.Host == r.host })
	// a tiebreaker has no pool, unless it was a copy with a disk before:
	// then its class names the pool of the LV that must be removed
	described.Diskless = volume.Copies[index].Tiebreaker
	if class := volume.Copies[index].Class; !described.Diskless || class != "" {
		pool, err := poolOf(config, id, class)
		if err != nil {
			return described, err
		}
		described.Pool = pool
	}
	if volume.Minor == nil {
		return described, nil // replicas 0: a plain LV
	}
	resource := &storage.Resource{Name: lv, Minor: *volume.Minor, ResyncRate: config.Replication.ResyncRate}
	for _, volumeCopy := range volume.Copies {
		node := storage.Node{ID: volumeCopy.Host}
		if host, ok := copied.Hosts[volumeCopy.Host]; ok {
			node.Host = host.Name
		}
		if !volumeCopy.Tiebreaker {
			peerPool, err := poolOf(config, id, volumeCopy.Class)
			if err != nil {
				return described, err
			}
			// a pool is "<volume group>/<thin pool>", and the LV's device is
			// in the volume group
			group, _, _ := strings.Cut(peerPool, "/")
			node.Disk = "/dev/" + group + "/" + lv
		}
		resource.Nodes = append(resource.Nodes, node)
	}
	described.Resource = resource
	return described, nil
}

// poolOf returns the thin pool of a copy's storage class. The state refuses
// a configuration that lacks a class which a volume uses, so a missing
// class is a bug. poolOf reports it clearly instead of passing an empty
// pool to lvcreate.
func poolOf(config clusterspec.Config, volumeID, class string) (string, error) {
	storageClass, ok := config.StorageClasses[class]
	if !ok || storageClass.Pool == "" {
		return "", fmt.Errorf("volume %s: the configuration has no storage class %s with a pool", volumeID, class)
	}
	return storageClass.Pool, nil
}

// placedHere reports whether the state has a copy of the volume on this host
// in the recorded data generation.
func (r *Reconciler) placedHere(copied *state.State, volume storage.Volume) bool {
	current, ok := copied.Volumes[volume.ID]
	if !ok || !slices.ContainsFunc(current.Copies, func(c state.VolumeCopy) bool { return c.Host == r.host }) {
		return false
	}
	return names.VolumeName(volume.ID, current.DataGeneration) == volume.LV
}

// retiredHere finds the retired copy in the state that matches the copy
// recorded on this host.
func (r *Reconciler) retiredHere(copied *state.State, volume storage.Volume) (state.RetiredCopy, bool) {
	for _, retired := range copied.Retired {
		if retired.Host == r.host && retired.Volume == volume.ID &&
			names.VolumeName(volume.ID, retired.DataGeneration) == volume.LV {
			return retired, true
		}
	}
	return state.RetiredCopy{}, false
}
