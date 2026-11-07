package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/webcodingcat/clowder/internal/hostrole"
	"github.com/webcodingcat/clowder/internal/state"
)

func TestAFollowerCatchesUpWithTheLeadersCommitIndex(t *testing.T) {
	cluster := newTestCluster(t)
	cluster.clusters[0].setIndex(42)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() {
		time.Sleep(50 * time.Millisecond)
		cluster.clusters[1].setIndex(42) // as replication would
	}()
	if err := cluster.peers[1].CatchUpWithLeader(ctx); err != nil {
		t.Fatal(err)
	}
	if err := cluster.peers[0].CatchUpWithLeader(ctx); err != nil {
		t.Errorf("the leader catches up without asking: %v", err)
	}
}

// A leader that has just stepped down answers 503 while the follower still
// names it; the follower asks again until it learns the next leader.
func TestAFollowerCatchesUpPastALeaderThatSteppedDown(t *testing.T) {
	cluster := newTestCluster(t)
	follower := cluster.clusters[1]
	follower.mu.Lock()
	follower.leaderID = workerHost // not the leader: it answers 503
	follower.mu.Unlock()
	go func() {
		time.Sleep(100 * time.Millisecond)
		follower.mu.Lock()
		follower.leaderID = 0
		follower.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := cluster.peers[1].CatchUpWithLeader(ctx); err != nil {
		t.Fatal(err)
	}
}

// The leader's refusal stays a refusal through the follower that forwards
// the command; the caller tells it from a command that did not commit.
func TestAProposalThroughTheLeaderKeepsItsRefusal(t *testing.T) {
	cluster := newTestCluster(t)
	cluster.clusters[0].Read(func(current *state.State) {
		current.Hosts[workerHost] = &state.Host{ID: workerHost, Name: "node2", Roles: rolesOf(workerHost), Phase: state.Active}
	})
	ctx := context.Background()
	cmd, err := state.NewCommand(state.KindSetHostPhase, time.Time{}, state.CLIActor("alice"),
		state.SetHostPhase{Host: 7, Phase: state.Draining})
	if err != nil {
		t.Fatal(err)
	}
	var refused *state.Refused
	if _, err := cluster.peers[1].Propose(ctx, cmd); !errors.As(err, &refused) || !strings.Contains(err.Error(), "no host 7") {
		t.Errorf("a command the state refuses: %v", err)
	}
	// a worker may not send the CLI's write: the leader forbids it, the
	// state never sees it
	if _, err := cluster.peers[workerHost].Propose(ctx, cmd); statusOf(err) != http.StatusForbidden || errors.As(err, &refused) {
		t.Errorf("the CLI's write from a worker: %v", err)
	}
}

func TestOnlyAControlPlaneHostAsksHowFarCopiesAre(t *testing.T) {
	cluster := newTestCluster(t)
	ctx := context.Background()
	if _, err := cluster.peers[0].Progress(ctx, 1); err != nil {
		t.Errorf("host 0 asks host 1: %v", err)
	}
	if _, err := cluster.peers[workerHost].Progress(ctx, 0); statusOf(err) != http.StatusForbidden ||
		!strings.Contains(err.Error(), "ask host 0 how far its copies are") {
		t.Errorf("a worker asks host 0: %v", err)
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
		state.AddJoinToken{ID: "token", Hash: strings.Repeat("ab", 32), TTL: time.Hour, Roles: hostrole.DefaultJoin})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cluster.peers[1].Propose(context.Background(), cmd); err != nil {
		t.Errorf("the write did not wait for the leader: %v", err)
	}
}
