package statetest

import (
	"testing"

	"github.com/webcodingcat/clowder/internal/state"
)

// recorder is a test whose failures are recorded, not reported.
type recorder struct {
	testing.TB
	cleanups []func()
	failed   bool
}

func (r *recorder) Cleanup(cleanup func()) { r.cleanups = append(r.cleanups, cleanup) }
func (r *recorder) Errorf(string, ...any)  { r.failed = true }
func (r *recorder) Helper()                {}

// end runs the cleanups, as the end of a test would.
func (r *recorder) end() {
	for _, cleanup := range r.cleanups {
		cleanup()
	}
}

func TestAWriteIntoTheCopyFailsTheTest(t *testing.T) {
	current := &state.State{Stacks: map[string]*state.Stack{"default/shop": {Name: "shop"}}}
	test := &recorder{TB: t}
	copied, err := ReadOnlyCopy(test, current)
	if err != nil {
		t.Fatal(err)
	}
	copied.Stacks["default/shop"].WaitReason = "written by a loop"
	test.end()
	if !test.failed {
		t.Error("a write into the copy went unnoticed")
	}
}

func TestReadingTheCopyPasses(t *testing.T) {
	current := &state.State{Stacks: map[string]*state.Stack{"default/shop": {Name: "shop"}}}
	test := &recorder{TB: t}
	copied, err := ReadOnlyCopy(test, current)
	if err != nil {
		t.Fatal(err)
	}
	_ = copied.Stacks["default/shop"].Name
	current.Stacks["default/shop"].WaitReason = "a later change of the state itself"
	test.end()
	if test.failed {
		t.Error("reading the copy failed the test")
	}
}
