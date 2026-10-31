package state

import (
	"errors"
	"slices"
	"testing"
)

// changeAt is a change of a stack made by the command at index.
func changeAt(index uint64, name string) Change {
	return Change{Kind: "Stack", Namespace: "default", Name: name, ResourceVersion: index}
}

func TestTheRingAnswersFromWhereItIsComplete(t *testing.T) {
	var ring ChangeRing
	ring.Reset(10) // started from a snapshot at index 10
	if _, err := ring.Since(9); !errors.Is(err, ErrChangesGone) {
		t.Errorf("before the snapshot: %v", err)
	}
	if changes, err := ring.Since(10); err != nil || len(changes) != 0 {
		t.Errorf("at the snapshot: %v, %v", changes, err)
	}
	ring.Add([]Change{changeAt(11, "shop"), changeAt(11, "blog")})
	ring.Add(nil) // a command that changed nothing
	ring.Add([]Change{changeAt(13, "shop")})
	if changes, err := ring.Since(11); err != nil || !slices.Equal(changes, []Change{changeAt(13, "shop")}) {
		t.Errorf("since 11: %v, %v", changes, err)
	}
	if changes, err := ring.Since(10); err != nil || len(changes) != 3 {
		t.Errorf("since 10: %v, %v", changes, err)
	}
}

func TestTheRingDropsTheOldestAndSaysSo(t *testing.T) {
	var ring ChangeRing
	for index := uint64(1); index <= ChangeRingSize; index++ {
		ring.Add([]Change{changeAt(index, "shop")})
	}
	if _, err := ring.Since(0); err != nil {
		t.Errorf("a full ring: %v", err)
	}
	// one command with two changes pushes out the first two
	ring.Add([]Change{changeAt(ChangeRingSize+1, "shop"), changeAt(ChangeRingSize+1, "blog")})
	if _, err := ring.Since(1); !errors.Is(err, ErrChangesGone) {
		t.Errorf("since 1, dropped: %v", err)
	}
	if changes, err := ring.Since(2); err != nil || len(changes) != ChangeRingSize || changes[0] != changeAt(3, "shop") {
		t.Errorf("since 2: %d changes, %v", len(changes), err)
	}
}
