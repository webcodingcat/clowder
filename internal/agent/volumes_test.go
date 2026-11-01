package agent

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/webcodingcat/clowder/internal/storage"
)

func TestTheRecordKeepsEachCopyOnce(t *testing.T) {
	runner, _, _, _ := newTestRunner(t)
	first := storage.Volume{ID: "default/db", LV: dbLV(1), Pool: "vg-fast/thin", SizeBytes: 1 << 30}
	second := storage.Volume{ID: "default/db", LV: dbLV(2), Pool: "vg-fast/thin", SizeBytes: 1 << 30}
	logs := storage.Volume{ID: "default/logs", LV: "logs-1", Pool: "vg-fast/thin", SizeBytes: 1 << 20}
	if err := runner.record([]storage.Volume{second, logs}); err != nil {
		t.Fatal(err)
	}
	grown := first
	grown.SizeBytes = 2 << 30
	// recorded again, as each pass does, and once grown: still one entry
	for _, volume := range []storage.Volume{first, first, grown} {
		if err := runner.record([]storage.Volume{volume}); err != nil {
			t.Fatal(err)
		}
	}

	recorded, err := runner.recordedVolumes()
	if err != nil {
		t.Fatal(err)
	}
	db := recorded["default/db"]
	if len(recorded) != 2 || len(db) != 2 || len(recorded["default/logs"]) != 1 {
		t.Fatalf("recorded %+v", recorded)
	}
	if db[0].LV != first.LV || db[0].SizeBytes != grown.SizeBytes || db[1].LV != second.LV {
		t.Errorf("the copies of db, sorted by LV, the first grown: %+v", db)
	}

	if err := runner.writeRecord("default/db", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(runner.files.volumeRecord("default/db")); !os.IsNotExist(err) {
		t.Errorf("the record without copies stayed: %v", err)
	}
}

func TestAHostWithoutRecordsHasNoCopies(t *testing.T) {
	runner, _, _, _ := newTestRunner(t)
	recorded, err := runner.recordedVolumes()
	if err != nil || len(recorded) != 0 {
		t.Errorf("recorded %v, %v", recorded, err)
	}
}

func TestARecordThatDoesNotReadIsNeitherUsedNorOverwritten(t *testing.T) {
	runner, cluster, _, storage := newTestRunner(t)
	runner.pass(context.Background())
	file := runner.files.volumeRecord("default/db")
	if err := os.WriteFile(file, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.recordedVolumes(); err == nil {
		t.Error("the copies were listed without the broken record")
	}
	// overwriting it would forget the copies it lists, and with them data
	placeDB(cluster, 2)
	runner.pass(context.Background())
	if content, err := os.ReadFile(file); err != nil || string(content) != "{not json" {
		t.Errorf("the broken record was overwritten: %q, %v", content, err)
	}
	if storage.count("ensure "+dbLV(2)) != 0 || storage.count("remove "+dbLV(1)) != 0 {
		t.Errorf("storage calls %q", storage.lines)
	}
}

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
