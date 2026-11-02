package agent

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestTrimRunsOnceAWeekAcrossRestarts(t *testing.T) {
	runner, _, _, storage := newTestRunner(t)
	runner.pass(context.Background())
	done, cancel := context.WithCancel(context.Background())
	cancel() // one round of the loop, then it ends
	trims := func() int {
		runner.trim(done)
		return storage.count("trim " + dbLV(1))
	}

	if got := trims(); got != 1 {
		t.Fatalf("%d trims at the first start, want 1", got)
	}
	if got := trims(); got != 1 {
		t.Errorf("%d trims after a restart within the week, want 1", got)
	}
	weekAgo := time.Now().Add(-trimInterval)
	if err := os.Chtimes(runner.files.lastTrim(), weekAgo, weekAgo); err != nil {
		t.Fatal(err)
	}
	storage.trimFails = true
	if got := trims(); got != 2 {
		t.Errorf("%d trims a week later, want 2", got)
	}
	// a failed trim counts too: the next try is in a week, not in an hour
	if got := trims(); got != 2 {
		t.Errorf("%d trims after a failed trim, want 2", got)
	}
}
