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
	if _, err := node.Propose(ctx, setCutoff(t)); err != nil {
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
	if !current.CLICutoff.Equal(traceCutoff) || current.Hosts[0] == nil {
		t.Errorf("cutoff %v, hosts %v", current.CLICutoff, current.Hosts)
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
	index, err := nodes[0].Propose(ctx, setCutoff(t))
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
	if !cutoffSet(recovered) {
		t.Error("the cutoff is gone after the recover")
	}
	members, err := recovered.Members()
	if err != nil || len(members) != 1 || !members[2] {
		t.Errorf("members %v, %v; want host 2 alone as a voter", members, err)
	}
}

// TestANonVoterAheadOfTheRecoveredMemberFollowsItWithAFreshLog is the
// rejoin of a worker after clowderd recover. Voter 2 stops first; voters 0
// and 1 go on in a higher term and commit an entry that only they and
// non-voter 3 hold. Then 0 and 1 are gone for good, and 2 recovers. Host 3
// sets its log aside and follows 2 as a fresh member: it holds 2's state,
// without the entry 2 never had, and 2 keeps its lead.
func TestANonVoterAheadOfTheRecoveredMemberFollowsItWithAFreshLog(t *testing.T) {
	cluster := newTestCluster(t)
	var nodes []*Node
	var dirs, addresses []string
	for id := range state.HostID(4) {
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
	if err := nodes[0].AddNonvoter(3, addresses[3]); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	index, err := nodes[0].Propose(ctx, bootstrapCommand(t))
	if err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for _, node := range nodes[1:] {
		if err := node.WaitIndex(waitCtx, index); err != nil {
			t.Fatal(err)
		}
	}

	if err := nodes[2].Close(); err != nil {
		t.Fatal(err)
	}
	if err := nodes[0].TransferLeadership(); err != nil {
		t.Fatal(err)
	}
	waitForLeader(t, nodes[1])
	index, err = nodes[1].Propose(ctx, setCutoff(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := nodes[3].WaitIndex(waitCtx, index); err != nil || !cutoffSet(nodes[3]) {
		t.Fatalf("the non-voter lacks the entry of the old majority: %v", err)
	}
	for _, id := range []int{0, 1, 3} {
		if err := nodes[id].Close(); err != nil {
			t.Fatal(err)
		}
	}

	if err := Recover(Config{Dir: dirs[2], ID: 2, Advertise: addresses[2]}); err != nil {
		t.Fatal(err)
	}
	recovered := cluster.open(2, dirs[2], addresses[2])
	defer func() { _ = recovered.Close() }()
	waitForLeader(t, recovered)
	rejoined := cluster.open(3, t.TempDir(), addresses[3]) // its log set aside
	defer func() { _ = rejoined.Close() }()
	if err := recovered.AddNonvoter(3, addresses[3]); err != nil {
		t.Fatal(err)
	}
	var want uint64
	recovered.Read(func(current *state.State) { want = current.Index })
	if err := rejoined.WaitIndex(waitCtx, want); err != nil {
		t.Fatal(err)
	}
	if cutoffSet(rejoined) || !recovered.IsLeader() {
		t.Errorf("cutoff on the rejoined host %v, the recovered host leads %v; want the recovered host's state "+
			"and lead", cutoffSet(rejoined), recovered.IsLeader())
	}
	rejoined.Read(func(current *state.State) {
		if current.Hosts[0] == nil {
			t.Error("the rejoined host lacks the bootstrap")
		}
	})
}
