// Package statetest helps the tests of the loops that read the state.
package statetest

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/webcodingcat/clowder/internal/state"
)

// ReadOnlyCopy copies current as raftnode.Node.SharedCopy does, and fails
// the test at its end if anything wrote into the copy. In the agent, all
// loops share one copy per change, so a write there would show every other
// loop a state that never existed. The copy also shares its specs and
// encrypted values with the state itself (state.State.Clone), so a write
// into those would change the state behind Raft's back. Go cannot make the
// copy read-only, so the tests of the loops check it instead, through its
// JSON.
func ReadOnlyCopy(t testing.TB, current *state.State) *state.State {
	t.Helper()
	copied := current.Clone()
	handedOut, err := json.Marshal(copied)
	if err != nil {
		t.Fatalf("encode the shared copy of the state: %v", err)
	}
	t.Cleanup(func() {
		atEnd, err := json.Marshal(copied)
		if err != nil {
			t.Errorf("encode the shared copy of the state: %v", err)
			return
		}
		if !bytes.Equal(handedOut, atEnd) {
			t.Errorf("a loop wrote into the shared copy of the state (index %d)", copied.Index)
		}
	})
	return copied
}
