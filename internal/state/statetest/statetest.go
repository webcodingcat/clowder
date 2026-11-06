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
// loop a state that never existed. Go cannot make the copy read-only, so
// the tests of the loops check it instead. The comparison uses JSON,
// because that is all that Copy copies.
func ReadOnlyCopy(t testing.TB, current *state.State) (*state.State, error) {
	t.Helper()
	copied, err := current.Copy()
	if err != nil {
		return nil, err
	}
	handedOut, err := json.Marshal(copied)
	if err != nil {
		return nil, err
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
	return copied, nil
}
