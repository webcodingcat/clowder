package state

import (
	"fmt"
	"maps"
	"slices"

	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/resource"
)

// RecoverRemap is clowderd recover's command, on the one host left of a
// cluster that lost its majority for good: the other hosts were powered off
// and labeled retired at Hetzner, and Raft runs with the survivor alone.
// It makes the state fit (applyRecoverRemap).
type RecoverRemap struct {
	Survivor HostID `json:"survivor"`
}

// recoveredBy is the error of the operations a recover ended.
const recoveredBy = "clowderd recover ended it"

// applyRecoverRemap makes every host but the survivor removed (its
// certificate revoked, the survivor to forget it as a DRBD peer). Of the
// volumes, a copy on a removed host is gone. A volume of replicas 1 and
// more keeps a synced copy with a disk on the survivor; one without waits
// for a restore (VolumeRestorePending): a tiebreaker of it there is
// retired, an unsynced copy there is kept, as the state never removes
// what may hold older data a human may want. A volume of replicas 0 whose LV was on
// a removed host starts anew, as after a failover. Every stack that is not
// on the survivor, or names a volume without data, is placed anew; every
// running operation ends as failed. The survivor holds the Floating IP and
// is the only voter. The cluster is no longer highly available: it grows
// again as a new one does, and RecoveredAt raises an alert until it is.
//
// Only the leader proposes it (clowderd recover, on the member it just
// recovered); the API forwards no such command. A second one changes
// nothing, so recover can run again after a failure.
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
	}
	gone := s.others(payload.Survivor)
	if len(gone) == 0 && !s.RecoveredAt.IsZero() {
		return nil, errUnchanged
	}
	return func() {
		for _, id := range gone {
			s.remove(s.Hosts[id], cmd.At)
		}
		// whatever the others thought of it (failed, draining), it is the
		// one host that runs stacks now
		survivor.Phase, survivor.Voter, survivor.Leaving = Active, true, false
		s.Ingress = &payload.Survivor
		s.HA, s.RecoveredAt = false, cmd.At
		s.endOperations()
		lost := s.remapVolumes(payload.Survivor)
		s.unplaceStacks(payload.Survivor, lost)
	}, nil
}

// others are the hosts not removed but the one given, sorted.
func (s *State) others(id HostID) []HostID {
	var others []HostID
	for _, other := range slices.Sorted(maps.Keys(s.Hosts)) {
		if other != id && s.Hosts[other].Phase != Removed {
			others = append(others, other)
		}
	}
	return others
}

// endOperations ends every running operation as failed and frees its
// locks: its hosts are gone, and the survivor starts over.
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
	// every lock belongs to an operation that ran (Locks)
	clear(s.Locks)
	s.forgetFinished()
}

// remapVolumes keeps of each volume what the survivor holds (see
// applyRecoverRemap) and returns the IDs of the volumes that wait for a
// restore.
func (s *State) remapVolumes(survivor HostID) map[string]bool {
	lost := map[string]bool{}
	for _, id := range slices.Sorted(maps.Keys(s.Volumes)) {
		volume := s.Volumes[id]
		if volume.Status != VolumeOK {
			continue // pending has no copy yet; restore-pending is from an earlier recover
		}
		index := slices.IndexFunc(volume.Copies, func(c VolumeCopy) bool { return c.Host == survivor })
		var kept *VolumeCopy
		if index >= 0 {
			kept = &volume.Copies[index]
		}
		switch {
		case volume.Spec.Replicas == 0 && kept != nil:
			continue
		case volume.Spec.Replicas == 0:
			// as after a failover: a new, empty LV with the next stack
			volume.Copies, volume.Status, volume.Initialized = nil, VolumePending, false
			volume.DataGeneration++
		case kept != nil && !kept.Tiebreaker && kept.Synced:
			volume.Copies = []VolumeCopy{*kept}
		case kept != nil && !kept.Tiebreaker:
			// unsynced: it may hold older data, which a human may want; it
			// stays a copy, so get volumes shows it and a delete removes it
			volume.Copies, volume.Status = []VolumeCopy{*kept}, VolumeRestorePending
			lost[id] = true
		default:
			if kept != nil {
				// a tiebreaker has no data to keep; retire gives up every copy
				// of the record it is handed, here the tiebreaker alone
				s.retire(&Volume{
					Meta: volume.Meta, DataGeneration: volume.DataGeneration, Minor: volume.Minor,
					Copies: []VolumeCopy{*kept},
				}, nil)
			}
			volume.Copies, volume.Status = nil, VolumeRestorePending
			lost[id] = true
		}
		s.changed(resource.KindVolume, &volume.Meta)
	}
	return lost
}

// unplaceStacks makes every stack pending that is not on the survivor or
// names a volume that waits for a restore: the placement places it anew.
func (s *State) unplaceStacks(survivor HostID, lost map[string]bool) {
	for _, id := range slices.Sorted(maps.Keys(s.Stacks)) {
		stack := s.Stacks[id]
		namesLost := slices.ContainsFunc(stack.Spec.VolumeNames(), func(name string) bool {
			return lost[names.ID(stack.Namespace, name)]
		})
		if stack.Host == nil || (*stack.Host == survivor && !namesLost) {
			continue
		}
		stack.Host, stack.Status, stack.UpError = nil, StackPending, ""
		stack.PushedOut, stack.AppliedSecrets, stack.MoveTo = nil, nil, nil
		s.changed(resource.KindStack, &stack.Meta)
	}
}
