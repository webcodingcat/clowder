// Package statetest helps the tests of the loops that read the state.
package statetest

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/webcodingcat/clowder/internal/state"
)

// ReadOnlyCopy copies current as raftnode.Node.SharedCopy does, and at the
// end of the test fails it if anything wrote into the copy. Go cannot make
// the copy read-only, so it compares the copy's JSON instead.
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
