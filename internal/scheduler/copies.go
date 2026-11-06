package scheduler

import (
	"cmp"
	"maps"
	"slices"

	"github.com/webcodingcat/clowder/internal/state"
)

// minTiebreakerHosts is the number of hosts that take tiebreakers that a
// cluster needs before a volume with two copies gets a tiebreaker on a
// third host. Only then does a volume get its second copy with a disk
// (addCopy).
const minTiebreakerHosts = 3

// keepCopies brings the copies of every placed volume to what its replicas
// ask for. It makes at most one change per volume and plan, the first of
// these that applies:
//   - add a missing copy (addCopy). The second copy with a disk waits for
//     a third host to take the tiebreaker (minTiebreakerHosts), because
//     two copies without a tiebreaker stop writing when either host fails.
//   - give up a surplus copy once the others are synced (dropSurplus).
//   - set or move the tiebreaker of a volume with two copies
//     (setTiebreaker).
//
// A new copy with a disk starts only while no other copy in the cluster
// syncs, so no link carries more than replication.resyncRate.
//
// A copy on a leaving host does not count. A copy on a draining host does,
// because draining is for maintenance. A volume that an operation holds is
// left to that operation.
func (p *planner) keepCopies() []state.Volume {
	var records []state.Volume
	for _, id := range slices.Sorted(maps.Keys(p.current.Volumes)) {
		volume := p.current.Volumes[id]
		switch {
		case volume.Status == state.VolumePending || volume.Status == state.VolumeRestorePending ||
			p.current.Locks[state.VolumeLock(id)] != 0:
			continue
		case volume.Spec.Replicas == 0:
			if record, ok := p.leaveThrownAway(volume); ok {
				records = append(records, record)
			}
			continue
		}
		for _, change := range []func(*state.Volume) (state.Volume, bool){p.addCopy, p.dropSurplus, p.setTiebreaker} {
			if record, ok := change(volume); ok {
				p.countTiebreakers(record)
				records = append(records, record)
				break
			}
		}
	}
	return records
}

// leaveThrownAway handles a volume of replicas 0 whose host no longer
// counts (counts), such as a host that leaves, and that no stack on that
// host uses. It gives the volume a new, empty LV in
// a new data generation, on the host that takes stacks and has the most
// free space in the pool. The data would be thrown away anyway when its
// stack moves, and the host can go only once nothing is left on it. A
// volume that a stack on the host uses moves with the stack.
func (p *planner) leaveThrownAway(volume *state.Volume) (state.Volume, bool) {
	if len(volume.Copies) != 1 || p.counts(volume.Copies[0].Host) {
		return state.Volume{}, false
	}
	if host, used := p.stackHostOf(volume); used && host == volume.Copies[0].Host {
		return state.Volume{}, false
	}
	best, bestFree, bestClass := state.HostID(0), int64(-1), ""
	for _, host := range p.stackHosts(nil) {
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

// resyncs tells whether a copy with a disk is catching up and so uses the
// resync limit. Only a copy on a running host can. A copy on a host that
// is down syncs nothing until the host is back. A copy of a volume that
// waits for a restore has nothing to sync from.
func (p *planner) resyncs(volume *state.Volume, volumeCopy state.VolumeCopy) bool {
	host, ok := p.current.Hosts[volumeCopy.Host]
	up := ok && state.Up(host.Phase)
	return volume.Status == state.VolumeOK && volume.Spec.Replicas > 0 && !volumeCopy.Synced && up &&
		p.counts(volumeCopy.Host)
}

// counts tells whether a copy on a host counts toward its volume's
// replicas. It does if the host runs stacks and stays, or if the host is
// down but comes back soon:
//   - a failed or fencing host counts, because the leader has not decided
//     yet;
//   - a fenced host counts for CopyWait, because the leader powers it on
//     again;
//   - a recovering host counts while it catches its copies up.
//
// The copies of a disabled host, or of one fenced for longer, are made
// anew elsewhere (addCopy). Once the host is back, the surplus copies go
// (dropSurplus).
func (p *planner) counts(id state.HostID) bool {
	host, ok := p.current.Hosts[id]
	if !ok {
		return false
	}
	switch host.Phase {
	case state.Active, state.Draining:
		return !host.Leaving
	case state.Failed, state.Fencing, state.Recovering:
		// even if it leaves: it moves its copies off once it is back
		return true
	case state.Fenced:
		return p.now.Sub(host.LastFenced()) < CopyWait
	}
	return false
}

// diskCopies returns a volume's copies with a disk.
func diskCopies(volume *state.Volume) []state.VolumeCopy {
	return slices.DeleteFunc(slices.Clone(volume.Copies), func(c state.VolumeCopy) bool { return c.Tiebreaker })
}

// addCopy places a missing copy, if the resync limit allows it. The copy
// goes to a host that takes stacks, preferring the one with the most free
// space in the pool. If that host holds the tiebreaker, the tiebreaker
// gets a disk.
func (p *planner) addCopy(volume *state.Volume) (state.Volume, bool) {
	counted := slices.DeleteFunc(diskCopies(volume), func(c state.VolumeCopy) bool { return !p.counts(c.Host) })
	if len(counted) >= volume.Spec.Replicas || p.syncing {
		return state.Volume{}, false
	}
	// only a new second copy waits; a copy that replaces one on a host
	// that leaves or is down joins a resource that still has that node
	if len(diskCopies(volume)) == 1 && len(p.tiebreakerHosts()) < minTiebreakerHosts {
		return state.Volume{}, false
	}
	type candidate struct {
		host       state.HostID
		class      string
		free       int64
		tiebreaker bool
	}
	var candidates []candidate
	for _, host := range p.stackHosts(nil) {
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
	// a host without a copy first, so that the tiebreaker stays; then the
	// most free space
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
	p.fitTiebreaker(&record, nil)
	p.syncing = true
	return record, true
}

// freeSpace returns the space that a pool on a host has left after what
// the plan placed in it.
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
// stays is synced and counts. This is the copy that a move or a leaving
// host left behind. It never gives up the copy where the stack runs.
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
	// first a copy that does not count, then one that is not synced,
	// then the one on the highest host ID
	dropped := slices.MinFunc(candidates, func(a, b state.VolumeCopy) int {
		return cmp.Or(
			cmp.Compare(boolRank(p.counts(a.Host)), boolRank(p.counts(b.Host))),
			cmp.Compare(boolRank(a.Synced), boolRank(b.Synced)),
			cmp.Compare(b.Host, a.Host),
		)
	})
	record := *volume
	record.Copies = slices.DeleteFunc(slices.Clone(volume.Copies), func(c state.VolumeCopy) bool { return c.Host == dropped.Host })
	p.fitTiebreaker(&record, &dropped)
	return record, true
}

// stackHostOf returns the host of the stack that names the volume, if that
// stack has one.
func (p *planner) stackHostOf(volume *state.Volume) (state.HostID, bool) {
	for _, id := range slices.Sorted(maps.Keys(p.current.Stacks)) {
		stack := p.current.Stacks[id]
		if stack.Namespace == volume.Namespace && stack.Host != nil && slices.Contains(stack.Spec.VolumeNames(), volume.Name) {
			return *stack.Host, true
		}
	}
	return 0, false
}

// setTiebreaker gives a volume the tiebreaker that its copies need
// (fitTiebreaker). It moves a tiebreaker away from a host whose copies no
// longer count (counts), such as a host that leaves. A draining host keeps
// its tiebreaker, as it keeps its copies. The old tiebreaker goes even if
// there is no new one, because a host that leaves must not keep it.
func (p *planner) setTiebreaker(volume *state.Volume) (state.Volume, bool) {
	record := *volume
	record.Copies = slices.DeleteFunc(slices.Clone(volume.Copies), func(c state.VolumeCopy) bool {
		return c.Tiebreaker && !p.counts(c.Host)
	})
	p.fitTiebreaker(&record, nil)
	if slices.Equal(record.Copies, volume.Copies) {
		return state.Volume{}, false
	}
	return record, true
}

// fitTiebreaker gives a volume's record the tiebreaker that its copies
// with a disk need. It changes the same record as the change that needs
// the tiebreaker. With a separate command, two copies would lack DRBD's
// quorum in between, and the failover of the stack's copy would hang if
// its host failed at that moment.
//   - Two copies with a disk get a tiebreaker, whatever the replicas. This
//     happens when a volume of replicas 1 moves, or when a volume of
//     replicas 3 has too few workers. If prefer's host takes tiebreakers,
//     the copy given up there becomes the tiebreaker. So no node joins or
//     leaves the DRBD resource. The host then detaches the disk and
//     removes the LV, which the copy's class still names. Otherwise the
//     tiebreaker goes to a host without a copy that holds the fewest
//     tiebreakers, whatever its roles. There is no tiebreaker while fewer
//     than minTiebreakerHosts hosts take tiebreakers.
//   - One copy with a disk loses its tiebreaker. With only two nodes, the
//     copy would lose its quorum along with the tiebreaker.
//   - A volume of replicas 3 with three copies with a disk loses its
//     tiebreaker too. The copies give each other their votes, and the
//     state allows a tiebreaker only next to two copies or in a volume of
//     replicas 2.
//
// In every other case, an existing tiebreaker stays. With three copies (a
// move of replicas 2), it does no harm, and it is in place once one of the
// copies goes.
func (p *planner) fitTiebreaker(record *state.Volume, prefer *state.VolumeCopy) {
	index := slices.IndexFunc(record.Copies, func(c state.VolumeCopy) bool { return c.Tiebreaker })
	disks := len(diskCopies(record))
	switch {
	case index >= 0 && (disks < 2 || disks > 2 && record.Spec.Replicas > 2):
		record.Copies = slices.Delete(slices.Clone(record.Copies), index, index+1)
		return
	case disks != 2 || index >= 0 || len(p.tiebreakerHosts()) < minTiebreakerHosts:
		return
	}
	if prefer != nil && p.takesTiebreakers(prefer.Host) {
		record.Copies = append(record.Copies, state.VolumeCopy{Host: prefer.Host, Class: prefer.Class, Tiebreaker: true})
		return
	}
	var candidates []state.HostID
	for _, host := range p.tiebreakerHosts() {
		if !slices.ContainsFunc(record.Copies, func(c state.VolumeCopy) bool { return c.Host == host }) &&
			!p.retiresHere(record.ID(), record.DataGeneration, host) {
			candidates = append(candidates, host)
		}
	}
	if len(candidates) == 0 {
		return
	}
	host := slices.MinFunc(candidates, func(a, b state.HostID) int {
		return cmp.Or(cmp.Compare(p.tiebreakers[a], p.tiebreakers[b]), cmp.Compare(a, b))
	})
	record.Copies = append(record.Copies, state.VolumeCopy{Host: host, Tiebreaker: true})
}

// countTiebreakers updates the plan's count of tiebreakers per host when a
// volume gets a new record. It takes away the tiebreakers of the volume's
// last record (planned earlier in this plan, or else the state's) and adds
// those of the new record.
func (p *planner) countTiebreakers(record state.Volume) {
	last, planned := p.planned[record.ID()]
	if old := p.current.Volumes[record.ID()]; !planned && old != nil {
		last = *old
	}
	p.planned[record.ID()] = record
	for _, volumeCopy := range last.Copies {
		if volumeCopy.Tiebreaker {
			p.tiebreakers[volumeCopy.Host]--
		}
	}
	for _, volumeCopy := range record.Copies {
		if volumeCopy.Tiebreaker {
			p.tiebreakers[volumeCopy.Host]++
		}
	}
}
