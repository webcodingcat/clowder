package raftnode

import (
	"context"
	"maps"
	"testing"
	"time"

	"github.com/webcodingcat/clowder/internal/state"
)

func TestTheLeaderChangesTheMembersAndHandsItsLeadOver(t *testing.T) {
	cluster := newTestCluster(t)
	var nodes []*Node
	var addresses []string
	for id := range state.HostID(3) {
		address := freeAddress(t)
		node := cluster.open(id, t.TempDir(), address)
		defer func() { _ = node.Close() }()
		nodes, addresses = append(nodes, node), append(addresses, address)
	}
	if err := nodes[0].Bootstrap(); err != nil {
		t.Fatal(err)
	}
	waitForLeader(t, nodes[0])
	if _, err := nodes[0].Propose(context.Background(), bootstrapCommand(t)); err != nil {
		t.Fatal(err)
	}
	for id := state.HostID(1); id < 3; id++ {
		if err := nodes[0].AddNonvoter(id, addresses[id]); err != nil {
			t.Fatal(err)
		}
	}
	checkMembers(t, nodes[0], map[state.HostID]bool{0: true, 1: false, 2: false})
	for id := state.HostID(1); id < 3; id++ {
		if err := nodes[0].AddVoter(id, addresses[id]); err != nil {
			t.Fatal(err)
		}
	}
	checkMembers(t, nodes[0], map[state.HostID]bool{0: true, 1: true, 2: true})

	if err := nodes[0].TransferLeadership(); err != nil {
		t.Fatal(err)
	}
	var next *Node
	deadline := time.Now().Add(15 * time.Second)
	for next == nil {
		for _, node := range nodes[1:] {
			if node.IsLeader() {
				next = node
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("no other host leads")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := next.RemoveMember(0); err != nil {
		t.Fatal(err)
	}
	if err := next.DemoteVoter(next.otherThan(t, 0)); err != nil {
		t.Fatal(err)
	}
	members, err := next.Members()
	if err != nil || len(members) != 2 {
		t.Fatalf("members %v, %v", members, err)
	}
	voting := 0
	for _, voter := range members {
		if voter {
			voting++
		}
	}
	if voting != 1 {
		t.Errorf("members %v: want one voter", members)
	}
}

// otherThan is the member that is neither this one nor left out.
func (n *Node) otherThan(t *testing.T, leftOut state.HostID) state.HostID {
	t.Helper()
	members, err := n.Members()
	if err != nil {
		t.Fatal(err)
	}
	for id := range members {
		if id != leftOut && serverID(id) != n.id {
			return id
		}
	}
	t.Fatalf("no other member in %v", members)
	return 0
}

func checkMembers(t *testing.T, node *Node, want map[state.HostID]bool) {
	t.Helper()
	got, err := node.Members()
	if err != nil || !maps.Equal(got, want) {
		t.Errorf("members %v, %v; want %v", got, err, want)
	}
}
