package api

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/webcodingcat/clowder/internal/state"
)

// fakeHealth counts the beats of each host and says every host was silent
// for as many seconds as its ID.
type fakeHealth struct {
	mu    sync.Mutex
	beats map[state.HostID]int
}

func (f *fakeHealth) Beat(host state.HostID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.beats[host]++
}

func (f *fakeHealth) Silence(host state.HostID) time.Duration {
	return time.Duration(host) * time.Second
}

func TestAHostBeatsToAVoterWhichTellsTheLeaderWhatItHeard(t *testing.T) {
	cluster := newTestCluster(t)
	ctx := context.Background()
	if err := cluster.peers[1].Beat(ctx, 0); err != nil {
		t.Fatal(err)
	}
	health := cluster.healths[0]
	health.mu.Lock()
	beats := health.beats[1]
	health.mu.Unlock()
	if beats != 1 {
		t.Errorf("host 0 heard host 1 %d times; the beat names its sender by its certificate", beats)
	}
	silence, err := cluster.peers[0].AskSilence(ctx, 1, 7)
	if err != nil || silence != 7*time.Second {
		t.Errorf("silence %v, %v", silence, err)
	}
}
