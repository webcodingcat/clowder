package raftnode

import (
	"context"
	"strings"
	"testing"
)

func TestAMemberThatNeverStartedReadsAnEmptyState(t *testing.T) {
	current, err := ReadState(t.TempDir())
	if err != nil || current.Keys != nil || len(current.Hosts) != 0 {
		t.Errorf("state %+v, %v", current, err)
	}
}

func TestTheStateIsReadOnlyWhileTheAgentIsStopped(t *testing.T) {
	cluster := newTestCluster(t)
	dir, address := t.TempDir(), freeAddress(t)
	node := cluster.open(0, dir, address)
	if err := node.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	waitForLeader(t, node)
	ctx := context.Background()
	if _, err := node.Propose(ctx, bootstrapCommand(t)); err != nil {
		t.Fatal(err)
	}
	// the state is in the snapshot and in the entry after it
	if err := node.raft.Snapshot().Error(); err != nil {
		t.Fatal(err)
	}
	if _, err := node.Propose(ctx, addUser(t, "alice", "a1")); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadState(dir); err == nil || !strings.Contains(err.Error(), "stop clowderd.service first") {
		t.Errorf("read while the member runs: %v", err)
	}
	if err := node.Close(); err != nil {
		t.Fatal(err)
	}
	current, err := ReadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := current.Users["alice"]; !ok || current.Hosts[0] == nil {
		t.Errorf("users %v, hosts %v", current.Users, current.Hosts)
	}
}
