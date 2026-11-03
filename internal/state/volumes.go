package state

import (
	"fmt"
	"maps"
	"slices"

	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/resource"
)

// MaxMinor is the highest DRBD minor: its port 10000 + minor stays in the
// range stackspec.ReservedPort keeps for DRBD (10000-10999); both change
// together.
const MaxMinor = 999

func findVolume(s *State, id string) *Meta {
	if volume, ok := s.Volumes[id]; ok {
		return &volume.Meta
	}
	return nil
}

// putVolumeResource makes a volume. It is placed together with the first
// stack that names it, on that stack's host; until then it waits
// (VolumePending). It needs no look at the volume's lock: it changes no
// volume there is, and only a volume there is can be locked.
func putVolumeResource(s *State, id string, payload PutResource) (func() *Meta, error) {
	spec, err := decodeSpec[resource.VolumeSpec](payload.Kind, id, payload.Spec)
	if err != nil {
		return nil, err
	}
	if stored, ok := s.Volumes[id]; ok {
		if err := stored.Spec.CheckUnchanged(spec); err != nil {
			return nil, fmt.Errorf("volume %s: %w", id, err)
		}
		return nil, errUnchanged
	}
	if err := spec.Check(); err != nil {
		return nil, fmt.Errorf("volume %s: %w", id, err)
	}
	if _, ok := s.Config.Spec.StorageClasses[spec.Class]; !ok {
		return nil, fmt.Errorf("volume %s: the cluster has no storage class %q", id, spec.Class)
	}
	generation := s.nextDataGeneration(id)
	if err := s.checkFreeLV(id, generation); err != nil {
		return nil, err
	}
	volume := &Volume{
		Meta: Meta{Namespace: payload.Namespace, Name: payload.Name, Owner: payload.Owner},
		Spec: spec, DataGeneration: generation, Status: VolumePending,
	}
	return func() *Meta {
		s.Volumes[id] = volume
		return &volume.Meta
	}, nil
}

// nextDataGeneration returns the data generation for a new volume: one
// above the highest retired copy of a deleted volume with the same ID. This
// way the new volume's LV cannot be mistaken for an old copy whose host may
// not have removed it yet.
func (s *State) nextDataGeneration(id string) int64 {
	generation := int64(1)
	for _, retired := range s.Retired {
		if retired.Volume == id {
			generation = max(generation, retired.DataGeneration+1)
		}
	}
	return generation
}

// checkFreeLV refuses an LV name that a volume or a retired copy has
// already: the name holds only 32 bits of a hash of the ID.
func (s *State) checkFreeLV(id string, generation int64) error {
	if generation > names.MaxDataGeneration {
		return fmt.Errorf("volume %s: data generation %d is beyond %d", id, generation, names.MaxDataGeneration)
	}
	lv := names.VolumeName(id, generation)
	for _, otherID := range slices.Sorted(maps.Keys(s.Volumes)) {
		other := s.Volumes[otherID]
		if names.VolumeName(otherID, other.DataGeneration) == lv {
			return fmt.Errorf("volume %s would have the LV of volume %s; choose another name", id, otherID)
		}
	}
	for _, retired := range s.Retired {
		if names.VolumeName(retired.Volume, retired.DataGeneration) == lv {
			return fmt.Errorf("volume %s would have the LV of a removed copy of %s; choose another name",
				id, retired.Volume)
		}
	}
	return nil
}

// removeVolumeResource refuses a volume a stack names; otherwise it
// retires the volume's copies, which their hosts remove with the data.
func removeVolumeResource(s *State, id string) (func(), error) {
	volume := s.Volumes[id]
	if stack := s.stackNaming(volume.Namespace, volume.Name, "", func(stack *Stack) []string {
		return stack.Spec.VolumeNames()
	}); stack != "" {
		return nil, fmt.Errorf("volume %s is stack %s's; change or delete the stack first", id, stack)
	}
	if err := s.checkUnlocked(VolumeLock(id), 0); err != nil {
		return nil, err
	}
	return func() {
		s.retire(volume, nil)
		delete(s.Volumes, id)
	}, nil
}

// PutVolume replaces a volume's record: where its copies are and how they
// are. The volume keeps its Meta and its spec, which only put-resource
// sets.
type PutVolume struct {
	Volume Volume `json:"volume"`
	// PlannedAt is the index of the state the leader planned with
	// (checkPlanned).
	PlannedAt uint64 `json:"planned_at"`
	// Operation is the leader's operation that makes the change, if any:
	// only the one that holds the volume's lock changes it.
	Operation uint64 `json:"operation,omitempty"`
}

// applyPutVolume takes the copies a record no longer has as retired: their
// hosts remove them, data and all. So a placed volume keeps at least one
// copy, and never goes back to pending: only the volume's delete gives up
// all of its data.
func applyPutVolume(s *State, _ Command, payload PutVolume) (func(), error) {
	volume := payload.Volume
	id := volume.ID()
	old, ok := s.Volumes[id]
	if !ok {
		return nil, fmt.Errorf("no volume %s", id)
	}
	if err := checkPlanned(resource.KindVolume, id, &old.Meta, payload.PlannedAt); err != nil {
		return nil, err
	}
	if err := s.checkUnlocked(VolumeLock(id), payload.Operation); err != nil {
		return nil, err
	}
	switch {
	case volume.Spec != old.Spec:
		return nil, fmt.Errorf("volume %s: its spec changes only with an apply", id)
	case old.Status != VolumePending && (volume.Status == VolumePending || len(volume.Copies) == 0):
		return nil, fmt.Errorf("volume %s holds data: it keeps a copy until it is deleted", id)
	}
	volume.Meta = old.Meta
	// only the hosts tell (set-volume-initialized); a new data generation
	// is a new LV, never initialized yet
	volume.Initialized = old.Initialized && volume.DataGeneration == old.DataGeneration
	volume.Copies = slices.Clone(volume.Copies)
	for i, volumeCopy := range volume.Copies {
		synced, err := keptSynced(old, volume.DataGeneration, volumeCopy)
		if err != nil {
			return nil, fmt.Errorf("volume %s: %w", id, err)
		}
		volume.Copies[i].Synced = synced
	}
	if sameJSON(*old, volume) {
		return nil, errUnchanged
	}
	if err := checkVolume(volume); err != nil {
		return nil, err
	}
	if volume.DataGeneration < old.DataGeneration {
		return nil, fmt.Errorf("volume %s: data generation %d is older than %d",
			id, volume.DataGeneration, old.DataGeneration)
	}
	if volume.DataGeneration != old.DataGeneration {
		if err := s.checkFreeLV(id, volume.DataGeneration); err != nil {
			return nil, err
		}
	}
	if volume.Minor != nil {
		if *volume.Minor < 0 || *volume.Minor > MaxMinor {
			return nil, fmt.Errorf("volume %s: minor %d is not from 0 to %d", id, *volume.Minor, MaxMinor)
		}
		for _, otherID := range slices.Sorted(maps.Keys(s.Volumes)) {
			if other := s.Volumes[otherID]; otherID != id && other.Minor != nil && *other.Minor == *volume.Minor {
				return nil, fmt.Errorf("volume %s: minor %d belongs to %s", id, *volume.Minor, otherID)
			}
		}
		if owner, ok := s.RetiredMinors[*volume.Minor]; ok && owner != id {
			return nil, fmt.Errorf("volume %s: minor %d is still the retired copies' of %s", id, *volume.Minor, owner)
		}
	}
	// fewer copies than replicas is allowed: a degraded volume waits for
	// its missing ones
	placed := map[HostID]bool{}
	for _, volumeCopy := range volume.Copies {
		if err := s.checkLiveHost(&volumeCopy.Host); err != nil {
			return nil, fmt.Errorf("volume %s: %w", id, err)
		}
		if placed[volumeCopy.Host] {
			return nil, fmt.Errorf("volume %s: two copies on host %d", id, volumeCopy.Host)
		}
		placed[volumeCopy.Host] = true
		// the retired copy's LV has the same name, and its host would take
		// the old data for the new copy's
		if slices.Contains(s.Retired, RetiredCopy{Volume: id, DataGeneration: volume.DataGeneration, Host: volumeCopy.Host}) {
			return nil, fmt.Errorf("volume %s: host %d still removes a copy it gave up", id, volumeCopy.Host)
		}
	}
	return func() {
		s.retire(old, &volume)
		s.Volumes[id] = &volume
		s.changed(resource.KindVolume, &volume.Meta)
	}, nil
}

// keptSynced is the Synced of a copy in a volume's new record: only its
// host tells (set-copy-synced), so a copy that stays what it was keeps the
// word, and any other starts unsynced. A copy with a disk cannot become a
// tiebreaker on its host: its LV would stay behind, as no retired copy
// names it.
func keptSynced(old *Volume, generation int64, volumeCopy VolumeCopy) (bool, error) {
	if generation != old.DataGeneration {
		return false, nil
	}
	index := slices.IndexFunc(old.Copies, func(c VolumeCopy) bool { return c.Host == volumeCopy.Host })
	switch {
	case index < 0:
		return false, nil
	case !old.Copies[index].Tiebreaker && volumeCopy.Tiebreaker:
		return false, fmt.Errorf("the copy on host %d has a disk; it cannot become a tiebreaker", volumeCopy.Host)
	case old.Copies[index].Tiebreaker != volumeCopy.Tiebreaker || old.Copies[index].Class != volumeCopy.Class:
		return false, nil
	}
	return old.Copies[index].Synced, nil
}

// retire marks the copies of old as retired. It skips each copy that kept
// (the volume's new record) still has on the same host with the same data
// generation.
func (s *State) retire(old *Volume, kept *Volume) {
	for _, volumeCopy := range old.Copies {
		if kept != nil && kept.DataGeneration == old.DataGeneration &&
			slices.ContainsFunc(kept.Copies, func(c VolumeCopy) bool { return c.Host == volumeCopy.Host }) {
			continue
		}
		retired := RetiredCopy{Volume: old.ID(), DataGeneration: old.DataGeneration, Host: volumeCopy.Host}
		if !slices.Contains(s.Retired, retired) {
			s.Retired = append(s.Retired, retired)
		}
		if old.Minor != nil {
			s.RetiredMinors[*old.Minor] = old.ID()
		}
	}
}

// ForgetCopy is a host telling that a retired copy of its is gone.
type ForgetCopy struct {
	Copy RetiredCopy `json:"copy"`
}

func applyForgetCopy(s *State, cmd Command, payload ForgetCopy) (func(), error) {
	if cmd.Actor != HostActor(payload.Copy.Host) {
		return nil, fmt.Errorf("only host %d itself forgets its copies", payload.Copy.Host)
	}
	index := slices.Index(s.Retired, payload.Copy)
	if index < 0 {
		return nil, fmt.Errorf("no retired copy of %s (data generation %d) on host %d",
			payload.Copy.Volume, payload.Copy.DataGeneration, payload.Copy.Host)
	}
	return func() {
		s.Retired = slices.Delete(s.Retired, index, index+1)
		s.forgetRetiredMinors()
	}, nil
}

// forgetRetiredMinors gives the minors back of the volumes that have no
// retired copy left: no host has their resource any more.
func (s *State) forgetRetiredMinors() {
	left := map[string]bool{}
	for _, retired := range s.Retired {
		left[retired.Volume] = true
	}
	maps.DeleteFunc(s.RetiredMinors, func(_ int, volume string) bool { return !left[volume] })
}

// SetVolumeInitialized is a host telling that its copy of a volume is
// primary and has its file system.
type SetVolumeInitialized struct {
	Volume         string `json:"volume"` // the volume's ID
	DataGeneration int64  `json:"data_generation"`
}

// applySetVolumeInitialized takes the word of a host that has a copy with
// a disk in the volume's data generation (diskCopyOf). It minds no lock:
// the host tells what its copy holds, which no operation can undo.
func applySetVolumeInitialized(s *State, cmd Command, payload SetVolumeInitialized) (func(), error) {
	volume, _, err := s.diskCopyOf(cmd.Actor, payload.Volume, payload.DataGeneration)
	if err != nil {
		return nil, err
	}
	if volume.Initialized {
		return nil, errUnchanged
	}
	return func() {
		volume.Initialized = true
		s.changed(resource.KindVolume, &volume.Meta)
	}, nil
}

// SetCopySynced is a host telling whether its copy of a volume holds the
// volume's current data (VolumeCopy.Synced).
type SetCopySynced struct {
	Volume         string `json:"volume"` // the volume's ID
	DataGeneration int64  `json:"data_generation"`
	Synced         bool   `json:"synced"`
}

// applySetCopySynced takes the word of a host about its copy with a disk in
// the volume's data generation, like set-volume-initialized. It minds no
// lock: a move waits to hear it.
func applySetCopySynced(s *State, cmd Command, payload SetCopySynced) (func(), error) {
	volume, index, err := s.diskCopyOf(cmd.Actor, payload.Volume, payload.DataGeneration)
	if err != nil {
		return nil, err
	}
	if volume.Copies[index].Synced == payload.Synced {
		return nil, errUnchanged
	}
	return func() {
		volume.Copies[index].Synced = payload.Synced
		s.changed(resource.KindVolume, &volume.Meta)
	}, nil
}

// diskCopyOf finds the copy with a disk that the actor, a host, has of a
// volume in its data generation: a host that took long to tell must not
// speak for a newer generation, whose LV is another one.
func (s *State) diskCopyOf(actor, volumeID string, generation int64) (*Volume, int, error) {
	volume, ok := s.Volumes[volumeID]
	if !ok {
		return nil, 0, fmt.Errorf("no volume %s", volumeID)
	}
	if volume.DataGeneration != generation {
		return nil, 0, fmt.Errorf("volume %s is in data generation %d, not %d",
			volumeID, volume.DataGeneration, generation)
	}
	index := slices.IndexFunc(volume.Copies, func(c VolumeCopy) bool {
		return HostActor(c.Host) == actor && !c.Tiebreaker
	})
	if index < 0 {
		return nil, 0, fmt.Errorf("%s has no copy of volume %s with a disk", actor, volumeID)
	}
	return volume, index, nil
}

// checkVolume holds what a volume's record must be on its own: the state is
// the last place to catch a wrong one.
func checkVolume(volume Volume) error {
	id := volume.ID()
	copies, tiebreakers := 0, 0
	for _, volumeCopy := range volume.Copies {
		if volumeCopy.Tiebreaker {
			tiebreakers++
		} else {
			copies++
		}
	}
	replicas := volume.Spec.Replicas
	if err := volume.Spec.Check(); err != nil {
		return fmt.Errorf("volume %s: %w", id, err)
	}
	switch {
	case volume.DataGeneration < 0 || volume.DataGeneration > names.MaxDataGeneration:
		// the generation is part of the LV's name, whose length has a limit
		return fmt.Errorf("volume %s: data generation %d is not from 0 to %d", id, volume.DataGeneration,
			names.MaxDataGeneration)
	case replicas == 0 && copies > 1:
		// replicas 0 is one plain LV on one host
		return fmt.Errorf("volume %s: %d copies for replicas 0", id, copies)
	case copies > replicas+1:
		// one more than replicas while a new copy takes an old one's
		// place (a move); DRBD's metadata has room for it (max-peers)
		return fmt.Errorf("volume %s: %d copies for %d replicas", id, copies, replicas)
	case tiebreakers > 0 && (replicas != 2 || tiebreakers > 1):
		// a tiebreaker gives two copies their third vote, and only them
		return fmt.Errorf("volume %s: %d tiebreakers for %d replicas", id, tiebreakers, replicas)
	case volume.Status == VolumePending && (len(volume.Copies) > 0 || volume.Minor != nil):
		// the minor comes with the first copy, from the placement
		return fmt.Errorf("volume %s: a pending volume has no copies and no DRBD minor", id)
	case volume.Status != VolumePending && (replicas == 0) != (volume.Minor == nil):
		return fmt.Errorf("volume %s: a DRBD minor exactly when it has replicas", id)
	}
	switch volume.Status {
	case VolumePending, VolumeOK:
		return nil
	default:
		return fmt.Errorf("volume %s: unknown status %q", id, volume.Status)
	}
}
