package agent

import (
	"context"
	"io"
	"log/slog"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/webcodingcat/clowder/internal/agent/agenttest"
	"github.com/webcodingcat/clowder/internal/state"
)

func TestARejoinDropsThePeersAndSetsTheRaftLogAsideAtTheNextStart(t *testing.T) {
	files := Files{Dir: t.TempDir()}
	if err := os.MkdirAll(files.raftDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	hostStorage := &agenttest.Storage{Dir: t.TempDir()}
	raftClosed := false
	rejoiner := &rejoiner{
		Storage: hostStorage, files: files, logger: logger, rejoined: make(chan struct{}),
		closeRaft: func() error {
			raftClosed = true
			return nil
		},
	}
	if err := rejoiner.Rejoin(context.Background(), []state.HostID{1, 3}); err != nil {
		t.Fatal(err)
	}
	if !raftClosed {
		t.Error("the old Raft member still runs when the rejoin answers")
	}
	if want := []string{"drop-peers [1 3]"}; !slices.Equal(hostStorage.Lines(), want) {
		t.Errorf("storage calls %q, want %q", hostStorage.Lines(), want)
	}
	select {
	case <-rejoiner.rejoined:
	default:
		t.Fatal("the agent does not end to rejoin")
	}

	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	if err := files.setRaftAsideToRejoin(now, logger); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(files.raftDir() + ".set-aside-20261106T120000Z"); err != nil {
		t.Errorf("the log is not set aside: %v", err)
	}
	for _, path := range []string{files.raftDir(), files.rejoinMarker()} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s is still there: %v", path, err)
		}
	}
	// the next start finds no marker and keeps the new log
	if err := os.MkdirAll(files.raftDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := files.setRaftAsideToRejoin(now.Add(time.Hour), logger); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(files.raftDir()); err != nil {
		t.Errorf("the new log moved: %v", err)
	}
}
