package state

import (
	"cmp"
	"errors"
	"slices"
)

// ChangeRingSize is how many changes an agent keeps for the watch. Hosts'
// reports about themselves are no change, so on a quiet cluster the ring
// covers hours.
const ChangeRingSize = 4096

// ErrChangesGone answers a watch from an index the ring no longer covers:
// the client lists afresh and watches from the list's index.
var ErrChangesGone = errors.New("the changes since that index are gone; list afresh")

// ChangeRing keeps the last changes the log made, oldest first, for the
// watch. It holds every change after index from: what came before is in
// the snapshot it started from, or dropped for room.
type ChangeRing struct {
	changes []Change
	from    uint64
}

// Reset empties the ring; it is complete from index on. An agent resets it
// to the state's index when it starts from a snapshot or installs one.
func (r *ChangeRing) Reset(index uint64) {
	r.changes, r.from = nil, index
}

// Add takes what one applied command changed, and drops the oldest changes
// beyond ChangeRingSize.
func (r *ChangeRing) Add(changes []Change) {
	r.changes = append(r.changes, changes...)
	if over := len(r.changes) - ChangeRingSize; over > 0 {
		// another change of the same command may stay, so the ring is
		// complete only after that command's index
		r.from = max(r.from, r.changes[over-1].ResourceVersion)
		r.changes = slices.Delete(r.changes, 0, over)
	}
}

// Since returns the changes after index, oldest first, or ErrChangesGone
// if the ring no longer holds all of them.
func (r *ChangeRing) Since(index uint64) ([]Change, error) {
	if index < r.from {
		return nil, ErrChangesGone
	}
	// the first change after index; versions only grow along the log
	first, _ := slices.BinarySearchFunc(r.changes, index+1, func(change Change, version uint64) int {
		return cmp.Compare(change.ResourceVersion, version)
	})
	return slices.Clone(r.changes[first:]), nil
}
