package state

import (
	"fmt"
	"maps"
	"slices"

	"github.com/webcodingcat/clowder/internal/hostrole"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/resource"
)

// RecoverRemap is the command of clowderd recover. It runs on a
// control-plane host, the survivor, of a cluster that lost the majority of
// its voters for good. The retired hosts were powered off and labeled
// retired at Hetzner, and Raft runs with the survivor as its only voter.
// The other hosts stay: workers that answered recover, which never voted.
// The command makes the state fit that (applyRecoverRemap).
type RecoverRemap struct {
	Survivor HostID   `json:"survivor"`
	Retired  []HostID `json:"retired,omitempty"`
	// Synced are the copies on the survivor and on the hosts that stay
	// that recover found synced there (DRBD's UpToDate).
	Synced []SyncedCopy `json:"synced,omitempty"`
}

// SyncedCopy is a copy that its host reported synced to clowderd recover.
// DataGeneration is the one the host holds; a copy of another generation
// holds other data and does not count.
type SyncedCopy struct {
	Volume         string `json:"volume"` // the volume's ID
	DataGeneration int64  `json:"data_generation"`
	Host           HostID `json:"host"`
}

// recoveredBy is the error of the operations that a recover ended.
const recoveredBy = "clowderd recover ended it"

// applyRecoverRemap marks the retired hosts as removed. It revokes their
// certificates, and the hosts that are left forget them as DRBD peers.
// Their volume copies are gone.
//
// A volume of replicas 1 or more keeps its copies on the hosts that are
// left. A copy with a disk is synced only if recover found it synced on
// its host (Synced), the survivor included. A volume
// without a synced copy waits for a restore (VolumeRestorePending): its
// tiebreakers are retired, and its unsynced copies are kept, because the
// state never removes what may hold older data that a human may want. A
// volume of replicas 0 whose LV was on a retired host starts anew, as after
// a failover.
//
// Every stack on a retired host, or that names a volume without data, is
// placed anew; the others run on. Every running operation fails. The
// survivor is the only voter. The cluster is no longer highly available.
// It grows again as a new cluster does, and RecoveredAt raises an alert
// until then.
//
// Only the leader proposes this command (clowderd recover), and the API
// forwards none. A second one with nothing new to retire changes nothing,
// so recover can run again.
func applyRecoverRemap(s *State, cmd Command, payload RecoverRemap) (func(), error) {
	if cmd.Actor != LeaderActor {
		return nil, fmt.Errorf("%s cannot remap the cluster after a recover; only clowderd recover does", cmd.Actor)
	}
	survivor, ok := s.Hosts[payload.Survivor]
	switch {
	case !ok:
		return nil, fmt.Errorf("no host %d", payload.Survivor)
	case survivor.Phase == Removed:
		return nil, fmt.Errorf("host %s is removed", survivor.Name)
	// it becomes the only voter
	case !survivor.Roles.Has(hostrole.ControlPlane):
		return nil, fmt.Errorf("host %s has the roles %s; only a %s host recovers", survivor.Name, survivor.Roles,
			hostrole.ControlPlane)
	}
	if err := s.checkRetiring(payload); err != nil {
		return nil, err
	}
	if len(payload.Retired) == 0 && !s.RecoveredAt.IsZero() {
		return nil, errUnchanged
	}
	return func() {
		for _, id := range payload.Retired {
			s.remove(s.Hosts[id], cmd.At)
		}
		// whatever the others thought of it (failed, draining), it is the
		// one voter in service now
		survivor.Phase, survivor.Voter, survivor.Leaving = Active, true, false
		for _, id := range s.others(payload.Survivor) {
			// a worker that stays answered recover, so it runs. Left failed,
			// it could be fenced before the new leader hears its first beat;
			// left fencing, nothing would move it on, as endOperations ends
			// its fence.
			if host := s.Hosts[id]; host.Phase == Failed || host.Phase == Fencing {
				host.Phase = Active
			}
		}
		if s.Ingress != nil && s.removed(*s.Ingress) {
			s.Ingress = nil
		}
		if s.Ingress == nil && survivor.Roles.Has(hostrole.Ingress) {
			s.Ingress = &payload.Survivor
		}
		s.HA, s.RecoveredAt = false, cmd.At
		s.endOperations()
		lost := s.remapVolumes(payload.Synced)
		s.unplaceStacks(lost)
	}, nil
}

// checkRetiring makes sure that the survivor is the only control-plane
// host left: another one could hold a vote that the state does not know of
// yet. The synced copies must be on the survivor or on hosts that stay.
func (s *State) checkRetiring(payload RecoverRemap) error {
	retired := map[HostID]bool{}
	for _, id := range payload.Retired {
		host, ok := s.Hosts[id]
		switch {
		case !ok:
			return fmt.Errorf("no host %d to retire", id)
		case id == payload.Survivor:
			return fmt.Errorf("host %s survives the recover and cannot be retired", host.Name)
		case host.Phase == Removed:
			return fmt.Errorf("host %s is removed already", host.Name)
		}
		retired[id] = true
	}
	for _, id := range s.others(payload.Survivor) {
		if host := s.Hosts[id]; !retired[id] && host.Roles.Has(hostrole.ControlPlane) {
			return fmt.Errorf("host %s is a %s host and is not retired; a recover leaves only one", host.Name,
				hostrole.ControlPlane)
		}
	}
	for _, synced := range payload.Synced {
		if host, ok := s.Hosts[synced.Host]; !ok || host.Phase == Removed || retired[synced.Host] {
			return fmt.Errorf("volume %s: host %d does not stay, so its copy cannot count", synced.Volume, synced.Host)
		}
	}
	return nil
}

// others returns the hosts that are not removed, except the one given,
// sorted.
func (s *State) others(id HostID) []HostID {
	var others []HostID
	for _, other := range slices.Sorted(maps.Keys(s.Hosts)) {
		if other != id && s.Hosts[other].Phase != Removed {
			others = append(others, other)
		}
	}
	return others
}

// removed also covers a host whose ID is free again.
func (s *State) removed(id HostID) bool {
	host, ok := s.Hosts[id]
	return !ok || host.Phase == Removed
}

// endOperations ends every running operation as failed and frees its
// locks, because its hosts may be gone and the survivor starts over.
func (s *State) endOperations() {
	for _, operation := range s.Operations {
		if operation.State != OpRunning && operation.State != OpRollingBack {
			continue
		}
		operation.State, operation.Error, operation.Owner = OpFailed, recoveredBy, nil
		if stack, ok := s.Stacks[moveStack(operation)]; ok {
			stack.MoveTo = nil
		}
	}
	// every lock belongs to an operation that was running (Locks)
	clear(s.Locks)
	s.forgetFinished()
}

// remapVolumes keeps the part of each volume that the hosts left hold (see
// applyRecoverRemap). The retired hosts are removed already. It returns the
// IDs of the volumes that wait for a restore.
func (s *State) remapVolumes(reported []SyncedCopy) map[string]bool {
	lost := map[string]bool{}
	for _, id := range slices.Sorted(maps.Keys(s.Volumes)) {
		volume := s.Volumes[id]
		if volume.Status != VolumeOK {
			continue // pending has no copy yet; restore-pending is from an earlier recover
		}
		var kept, tiebreakers []VolumeCopy
		synced := false
		for _, volumeCopy := range volume.Copies {
			switch {
			case s.removed(volumeCopy.Host):
			case volumeCopy.Tiebreaker:
				tiebreakers = append(tiebreakers, volumeCopy)
			default:
				// DRBD's disk state, as recover read it on each host, not the
				// state's flag: the survivor's state may be behind on any copy
				volumeCopy.Synced = slices.Contains(reported,
					SyncedCopy{Volume: id, DataGeneration: volume.DataGeneration, Host: volumeCopy.Host})
				synced = synced || volumeCopy.Synced
				kept = append(kept, volumeCopy)
			}
		}
		switch {
		case volume.Spec.Replicas == 0 && len(kept) > 0:
			// its one LV is on a host that is left
			continue
		case volume.Spec.Replicas == 0:
			// as after a failover: a new, empty LV with the next stack
			volume.Copies, volume.Status, volume.Initialized = nil, VolumePending, false
			volume.DataGeneration++
		case synced:
			volume.Copies = slices.Concat(kept, tiebreakers)
		default:
			if len(tiebreakers) > 0 {
				// a tiebreaker has no data to keep. retire gives up every
				// copy of the record it is handed, here only the tiebreakers.
				s.retire(&Volume{
					Meta: volume.Meta, DataGeneration: volume.DataGeneration, Minor: volume.Minor, Copies: tiebreakers,
				}, nil)
			}
			// unsynced copies may hold older data that a human may want.
			// They stay copies, so get volumes shows them and a delete
			// removes them.
			volume.Copies, volume.Status = kept, VolumeRestorePending
			lost[id] = true
		}
		s.changed(resource.KindVolume, &volume.Meta)
	}
	return lost
}

// unplaceStacks makes pending every stack on a removed host, or that names
// a volume waiting for a restore. The placement then places these stacks
// anew.
func (s *State) unplaceStacks(lost map[string]bool) {
	for _, id := range slices.Sorted(maps.Keys(s.Stacks)) {
		stack := s.Stacks[id]
		namesLost := slices.ContainsFunc(stack.Spec.VolumeNames(), func(name string) bool {
			return lost[names.ID(stack.Namespace, name)]
		})
		if stack.Host == nil || (!s.removed(*stack.Host) && !namesLost) {
			continue
		}
		stack.Host, stack.Status, stack.UpError = nil, StackPending, ""
		stack.PushedOut, stack.AppliedSecrets, stack.MoveTo = nil, nil, nil
		s.changed(resource.KindStack, &stack.Meta)
	}
}
