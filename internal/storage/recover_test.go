package storage

import (
	"context"
	"slices"
	"testing"
)

func TestDroppedPeersLeaveTheQuorumOffAndTheWritesResumed(t *testing.T) {
	host := &forgetHost{
		// clw-a is primary without quorum, its writes suspended; clw-b has
		// one of the peers; clw-c has the survivor's other peer only
		status: `[{"name": "clw-a", "suspended": true, "connections": [{"peer-node-id": 1}, {"peer-node-id": 2}]},
			{"name": "clw-b", "connections": [{"peer-node-id": 2}]},
			{"name": "clw-c", "connections": [{"peer-node-id": 3}]}]`,
	}
	driver := Driver{Run: host.run}
	if err := driver.DropPeers(context.Background(), []int{1, 2}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"drbdsetup status --json",
		"drbdsetup del-peer clw-a 1", "drbdsetup del-peer clw-a 2",
		"drbdsetup resource-options clw-a --quorum=off", "drbdadm resume-io clw-a",
		"drbdsetup del-peer clw-b 2", "drbdsetup resource-options clw-b --quorum=off",
	}
	if !slices.Equal(host.commands, want) {
		t.Errorf("commands %q\nwant %q", host.commands, want)
	}

	// again, once the peers are dropped: nothing to do
	host.status = `[{"name": "clw-a", "connections": []}, {"name": "clw-b", "connections": []}]`
	host.commands = nil
	if err := driver.DropPeers(context.Background(), []int{1, 2}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(host.commands, []string{"drbdsetup status --json"}) {
		t.Errorf("a second run: %q", host.commands)
	}
}
