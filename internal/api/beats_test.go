package api

import (
	"context"
	"strings"
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
	if err := cluster.apis[1].Beat(ctx, 0); err != nil {
		t.Fatal(err)
	}
	health := cluster.healths[0]
	health.mu.Lock()
	beats := health.beats[1]
	health.mu.Unlock()
	if beats != 1 {
		t.Errorf("host 0 heard host 1 %d times; the beat names its sender by its certificate", beats)
	}
	silence, err := cluster.apis[0].AskSilence(ctx, 1, 7)
	if err != nil || silence != 7*time.Second {
		t.Errorf("silence %v, %v", silence, err)
	}
}

func TestAFollowerCatchesUpWithTheLeadersCommitIndex(t *testing.T) {
	cluster := newTestCluster(t)
	cluster.clusters[0].setIndex(42)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() {
		time.Sleep(50 * time.Millisecond)
		cluster.clusters[1].setIndex(42) // as replication would
	}()
	if err := cluster.apis[1].CatchUpWithLeader(ctx); err != nil {
		t.Fatal(err)
	}
	if err := cluster.apis[0].CatchUpWithLeader(ctx); err != nil {
		t.Errorf("the leader catches up without asking: %v", err)
	}
}

func TestAWriteWaitsForALeaderDuringAnElection(t *testing.T) {
	cluster := newTestCluster(t)
	follower := cluster.clusters[1]
	follower.mu.Lock()
	follower.noLeader = true
	follower.mu.Unlock()
	go func() {
		time.Sleep(100 * time.Millisecond)
		follower.mu.Lock()
		follower.noLeader = false
		follower.mu.Unlock()
	}()
	cmd, err := state.NewCommand(state.KindAddJoinToken, time.Time{}, state.CLIActor("admin"),
		state.AddJoinToken{ID: "token", Hash: strings.Repeat("ab", 32), TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cluster.apis[1].Propose(context.Background(), cmd); err != nil {
		t.Errorf("the write did not wait for the leader: %v", err)
	}
}
