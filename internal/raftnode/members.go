package raftnode

import (
	"fmt"
	"strconv"

	"github.com/hashicorp/raft"

	"github.com/webcodingcat/clowder/internal/state"
)

// Members returns the hosts in Raft's latest configuration, committed or
// not. The value says whether the host votes.
func (n *Node) Members() (map[state.HostID]bool, error) {
	future := n.raft.GetConfiguration()
	if err := future.Error(); err != nil {
		return nil, fmt.Errorf("read Raft's members: %w", err)
	}
	members := map[state.HostID]bool{}
	for _, server := range future.Configuration().Servers {
		id, err := strconv.Atoi(string(server.ID))
		if err != nil {
			return nil, fmt.Errorf("read Raft's members: server ID %q is no host ID", server.ID)
		}
		members[state.HostID(id)] = server.Suffrage == raft.Voter
	}
	return members, nil
}

// AddNonvoter adds a host, at its Raft address, that follows the log and
// does not vote. A host that is already a member keeps its vote. Only the
// leader changes the members.
func (n *Node) AddNonvoter(id state.HostID, address string) error {
	err := n.raft.AddNonvoter(serverID(id), raft.ServerAddress(address), 0, proposeTimeout).Error()
	if err != nil {
		return fmt.Errorf("add host %d to Raft: %w", id, err)
	}
	return nil
}

// AddVoter makes a member a voter, or adds a host as a voter.
func (n *Node) AddVoter(id state.HostID, address string) error {
	err := n.raft.AddVoter(serverID(id), raft.ServerAddress(address), 0, proposeTimeout).Error()
	if err != nil {
		return fmt.Errorf("make host %d a Raft voter: %w", id, err)
	}
	return nil
}

// DemoteVoter takes a member's vote away and keeps it as a member.
func (n *Node) DemoteVoter(id state.HostID) error {
	if err := n.raft.DemoteVoter(serverID(id), 0, proposeTimeout).Error(); err != nil {
		return fmt.Errorf("take host %d's Raft vote: %w", id, err)
	}
	return nil
}

// RemoveMember takes a host out of Raft. A leader that removed itself would
// end its lead abruptly. So a leader hands over the lead first
// (TransferLeadership), and the next leader removes it.
func (n *Node) RemoveMember(id state.HostID) error {
	if err := n.raft.RemoveServer(serverID(id), 0, proposeTimeout).Error(); err != nil {
		return fmt.Errorf("remove host %d from Raft: %w", id, err)
	}
	return nil
}

// TransferLeadership hands the lead to another voter. It returns once that
// voter leads, or once the transfer failed. When the leading host is
// removed this way, the cluster needs no election after a timeout, as it
// would if the leader just went silent.
func (n *Node) TransferLeadership() error {
	if err := n.raft.LeadershipTransfer().Error(); err != nil {
		return fmt.Errorf("hand the Raft lead over: %w", err)
	}
	return nil
}
