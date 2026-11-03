package scheduler

import (
	"cmp"
	"maps"
	"slices"

	"github.com/webcodingcat/clowder/internal/state"
)

// minTiebreakerHosts is how many hosts that take stacks a cluster needs
// before a volume with two copies gets a tiebreaker on a third.
const minTiebreakerHosts = 3

// keepCopies brings the copies of every placed volume to what its replicas
// ask for, one change per volume and plan, in this order:
//   - a missing copy, on the host that takes stacks with the most free
//     space in the volume's pool; a tiebreaker there gets a disk;
//   - a surplus copy given up, once the others are synced: the one on a
//     host that leaves first, never the one where its stack runs;
//   - the tiebreaker of a volume with two copies, on a host without a
//     copy that holds the fewest tiebreakers, once there are three hosts
//     that take stacks; one on a host that no longer takes them moves.
//
// The resync limit: a new copy with a disk starts only while no other
// syncs, in the whole cluster. Each copy then resyncs at
// replication.resync_rate, and so no link carries more.
//
// A copy on a host that leaves does not count; one on a draining host
// does, as draining is for maintenance. A volume an operation holds is
// left to it.
func (p *planner) keepCopies() []state.Volume {
	var records []state.Volume
	for _, id := range slices.Sorted(maps.Keys(p.current.Volumes)) {
		volume := p.current.Volumes[id]
		switch {
		case volume.Status == state.VolumePending || p.current.Locks[state.VolumeLock(id)] != 0:
			continue
		case volume.Spec.Replicas == 0:
			if record, ok := p.leaveThrownAway(volume); ok {
				records = append(records, record)
			}
			continue
		}
		for _, change := range []func(*state.Volume) (state.Volume, bool){p.addCopy, p.dropSurplus, p.setTiebreaker} {
			if record, ok := change(volume); ok {
				records = append(records, record)
				break
			}
		}
	}
	return records
}

// leaveThrownAway gives a volume of replicas 0 that no stack runs with a new,
// empty LV elsewhere when its host leaves: its data is thrown away anyway
// when its stack moves, and the host can go only once nothing is on it.
// The new LV goes to the host that takes stacks with the most free space
// in the pool, in a new data generation. One a stack runs with moves with
// the stack.
func (p *planner) leaveThrownAway(volume *state.Volume) (state.Volume, bool) {
	if len(volume.Copies) != 1 || p.counts(volume.Copies[0].Host) {
		return state.Volume{}, false
	}
	if host, used := p.stackHostOf(volume); used && host == volume.Copies[0].Host {
		return state.Volume{}, false
	}
	best, bestFree, bestClass := state.HostID(0), int64(-1), ""
	for _, host := range p.activeHosts(nil) {
		class, pool, ok := p.fit(host, volume.Spec.Class, int64(volume.Spec.Size), nil)
		if free := p.freeSpace(host, pool); ok && free > bestFree {
			best, bestFree, bestClass = host, free, class
		}
	}
	if bestFree < 0 {
		return state.Volume{}, false
	}
	record := *volume
	record.DataGeneration++
	record.Copies = []state.VolumeCopy{{Host: best, Class: bestClass}}
	p.take(best, p.classes[bestClass].Pool, int64(volume.Spec.Size))
	return record, true
}

// resyncs tells whether a copy with a disk catches up, on a host that
// counts: it takes the resync limit.
func (p *planner) resyncs(volume *state.Volume, volumeCopy state.VolumeCopy) bool {
	return volume.Status != state.VolumePending && volume.Spec.Replicas > 0 && !volumeCopy.Synced &&
		p.counts(volumeCopy.Host)
}

// counts tells whether a copy on a host counts toward its volume's
// replicas: the host runs stacks and stays.
func (p *planner) counts(id state.HostID) bool {
	host, ok := p.current.Hosts[id]
	return ok && (host.Phase == state.Active || host.Phase == state.Draining) && !host.Leaving
}

// diskCopies are a volume's copies with a disk.
func diskCopies(volume *state.Volume) []state.VolumeCopy {
	return slices.DeleteFunc(slices.Clone(volume.Copies), func(c state.VolumeCopy) bool { return c.Tiebreaker })
}

// addCopy places a missing copy, if the resync limit lets it.
func (p *planner) addCopy(volume *state.Volume) (state.Volume, bool) {
	counted := slices.DeleteFunc(diskCopies(volume), func(c state.VolumeCopy) bool { return !p.counts(c.Host) })
	if len(counted) >= volume.Spec.Replicas || p.syncing {
		return state.Volume{}, false
	}
	type candidate struct {
		host       state.HostID
		class      string
		free       int64
		tiebreaker bool
	}
	var candidates []candidate
	for _, host := range p.activeHosts(nil) {
		index := slices.IndexFunc(volume.Copies, func(c state.VolumeCopy) bool { return c.Host == host })
		hasDisk := index >= 0 && !volume.Copies[index].Tiebreaker
		if hasDisk || p.retiresHere(volume.ID(), volume.DataGeneration, host) {
			continue
		}
		class, pool, ok := p.fit(host, volume.Spec.Class, int64(volume.Spec.Size), nil)
		if !ok {
			continue
		}
		candidates = append(candidates, candidate{host, class, p.freeSpace(host, pool), index >= 0})
	}
	if len(candidates) == 0 {
		return state.Volume{}, false
	}
	// a host without a copy first, so the tiebreaker stays; then the most
	// free space
	best := slices.MinFunc(candidates, func(a, b candidate) int {
		if a.tiebreaker != b.tiebreaker {
			return cmp.Compare(boolRank(a.tiebreaker), boolRank(b.tiebreaker))
		}
		return cmp.Or(cmp.Compare(b.free, a.free), cmp.Compare(a.host, b.host))
	})
	record := *volume
	record.Copies = slices.DeleteFunc(slices.Clone(volume.Copies), func(c state.VolumeCopy) bool { return c.Host == best.host })
	record.Copies = append(record.Copies, state.VolumeCopy{Host: best.host, Class: best.class})
	p.take(best.host, p.classes[best.class].Pool, int64(volume.Spec.Size))
	if best.tiebreaker {
		p.tiebreakers[best.host]--
	}
	p.syncing = true
	return record, true
}

// freeSpace is what a pool on a host has left beside what the plan placed
// in it.
func (p *planner) freeSpace(host state.HostID, pool string) int64 {
	free := p.current.Hosts[host].Pools[pool].SizeBytes
	for _, size := range p.allocated[host][pool] {
		free -= size
	}
	return free
}

// boolRank orders false before true.
func boolRank(value bool) int {
	if value {
		return 1
	}
	return 0
}

// dropSurplus gives up one copy beyond replicas, once every copy that
// stays is synced and counts: the copy that a move or a leaving host left
// behind.
func (p *planner) dropSurplus(volume *state.Volume) (state.Volume, bool) {
	disks := diskCopies(volume)
	if len(disks) <= volume.Spec.Replicas {
		return state.Volume{}, false
	}
	runsOn, used := p.stackHostOf(volume)
	var candidates []state.VolumeCopy
	for _, volumeCopy := range disks {
		if used && volumeCopy.Host == runsOn {
			continue
		}
		staying := slices.DeleteFunc(slices.Clone(disks), func(c state.VolumeCopy) bool { return c.Host == volumeCopy.Host })
		if !slices.ContainsFunc(staying, func(c state.VolumeCopy) bool { return !c.Synced || !p.counts(c.Host) }) {
			candidates = append(candidates, volumeCopy)
		}
	}
	if len(candidates) == 0 {
		return state.Volume{}, false
	}
	// one that does not count first, then one not synced, then the
	// highest host
	dropped := slices.MinFunc(candidates, func(a, b state.VolumeCopy) int {
		return cmp.Or(
			cmp.Compare(boolRank(p.counts(a.Host)), boolRank(p.counts(b.Host))),
			cmp.Compare(boolRank(a.Synced), boolRank(b.Synced)),
			cmp.Compare(b.Host, a.Host),
		)
	})
	record := *volume
	record.Copies = slices.DeleteFunc(slices.Clone(volume.Copies), func(c state.VolumeCopy) bool { return c.Host == dropped.Host })
	return record, true
}

// stackHostOf is the host of the stack that names the volume, if it has
// one.
func (p *planner) stackHostOf(volume *state.Volume) (state.HostID, bool) {
	for _, id := range slices.Sorted(maps.Keys(p.current.Stacks)) {
		stack := p.current.Stacks[id]
		if stack.Namespace == volume.Namespace && stack.Host != nil && slices.Contains(stack.Spec.VolumeNames(), volume.Name) {
			return *stack.Host, true
		}
	}
	return 0, false
}

// setTiebreaker puts the tiebreaker of a volume with two copies on a host
// that takes stacks, and moves one from a host that leaves or no longer
// runs stacks; a draining host keeps its tiebreaker, as it keeps its
// copies.
func (p *planner) setTiebreaker(volume *state.Volume) (state.Volume, bool) {
	if volume.Spec.Replicas != 2 || len(diskCopies(volume)) != 2 {
		return state.Volume{}, false
	}
	index := slices.IndexFunc(volume.Copies, func(c state.VolumeCopy) bool { return c.Tiebreaker })
	switch {
	case index >= 0 && p.counts(volume.Copies[index].Host):
		return state.Volume{}, false
	case index < 0 && len(p.activeHosts(nil)) < minTiebreakerHosts:
		return state.Volume{}, false
	}
	var candidates []state.HostID
	for _, host := range p.activeHosts(nil) {
		if !slices.ContainsFunc(volume.Copies, func(c state.VolumeCopy) bool { return c.Host == host }) &&
			!p.retiresHere(volume.ID(), volume.DataGeneration, host) {
			candidates = append(candidates, host)
		}
	}
	record := *volume
	record.Copies = slices.DeleteFunc(slices.Clone(volume.Copies), func(c state.VolumeCopy) bool { return c.Tiebreaker })
	if len(candidates) == 0 {
		if index < 0 {
			return state.Volume{}, false
		}
		// the old one goes even so: a host that leaves must not keep it
		p.tiebreakers[volume.Copies[index].Host]--
		return record, true
	}
	host := slices.MinFunc(candidates, func(a, b state.HostID) int {
		return cmp.Or(cmp.Compare(p.tiebreakers[a], p.tiebreakers[b]), cmp.Compare(a, b))
	})
	if index >= 0 {
		p.tiebreakers[volume.Copies[index].Host]--
	}
	p.tiebreakers[host]++
	record.Copies = append(record.Copies, state.VolumeCopy{Host: host, Tiebreaker: true})
	return record, true
}
