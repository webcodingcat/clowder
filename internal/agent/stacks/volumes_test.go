package stacks

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/webcodingcat/clowder/internal/agent/agenttest"
)

func TestTrimRunsOnceAWeekAcrossRestarts(t *testing.T) {
	runner, _, _, storage := newTestRunner(t)
	runner.pass(context.Background())
	done, cancel := context.WithCancel(context.Background())
	cancel() // one round of the loop, then it ends
	trims := func() int {
		runner.Trim(done)
		return storage.Count("trim " + agenttest.DBLV(1))
	}

	if got := trims(); got != 1 {
		t.Fatalf("%d trims at the first start, want 1", got)
	}
	if got := trims(); got != 1 {
		t.Errorf("%d trims after a restart within the week, want 1", got)
	}
	weekAgo := time.Now().Add(-trimInterval)
	if err := os.Chtimes(runner.files.LastTrim, weekAgo, weekAgo); err != nil {
		t.Fatal(err)
	}
	storage.Lock()
	storage.TrimFails = true
	storage.Unlock()
	if got := trims(); got != 2 {
		t.Errorf("%d trims a week later, want 2", got)
	}
	// a failed trim counts too: the next try is in a week, not in an hour
	if got := trims(); got != 2 {
		t.Errorf("%d trims after a failed trim, want 2", got)
	}
}
