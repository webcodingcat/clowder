package raftnode

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/webcodingcat/clowder/internal/state"
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

func TestARecoveredMemberLeadsAloneWithItsState(t *testing.T) {
	cluster := newTestCluster(t)
	var nodes []*Node
	var dirs, addresses []string
	for id := range state.HostID(3) {
		dir, address := t.TempDir(), freeAddress(t)
		nodes, dirs, addresses = append(nodes, cluster.open(id, dir, address)), append(dirs, dir), append(addresses, address)
	}
	if err := nodes[0].Bootstrap(); err != nil {
		t.Fatal(err)
	}
	waitForLeader(t, nodes[0])
	for id := 1; id < 3; id++ {
		if err := nodes[0].AddVoter(state.HostID(id), addresses[id]); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	if _, err := nodes[0].Propose(ctx, bootstrapCommand(t)); err != nil {
		t.Fatal(err)
	}
	index, err := nodes[0].Propose(ctx, addUser(t, "alice", "a1"))
	if err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := nodes[2].WaitIndex(waitCtx, index); err != nil {
		t.Fatal(err)
	}
	// hosts 0 and 1 are gone for good; host 2 alone has no quorum
	for _, node := range nodes {
		if err := node.Close(); err != nil {
			t.Fatal(err)
		}
	}
	survivor := Config{Dir: dirs[2], ID: 2, Advertise: addresses[2]}
	if err := Recover(survivor); err != nil {
		t.Fatal(err)
	}
	recovered := cluster.open(2, dirs[2], addresses[2])
	defer func() { _ = recovered.Close() }()
	waitForLeader(t, recovered)
	if got := users(recovered); len(got) != 1 || got[0] != "alice" {
		t.Errorf("users after the recover: %v", got)
	}
	members, err := recovered.Members()
	if err != nil || len(members) != 1 || !members[2] {
		t.Errorf("members %v, %v; want host 2 alone as a voter", members, err)
	}
}
