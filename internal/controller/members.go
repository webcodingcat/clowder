package controller

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/webcodingcat/clowder/internal/hostrole"
	"github.com/webcodingcat/clowder/internal/state"
)

// Members is Raft's configuration as the leader changes it
// (raftnode.Node). Raft changes its members one at a time.
type Members interface {
	// Members returns the hosts in Raft, each with whether it votes.
	Members() (map[state.HostID]bool, error)
	AddNonvoter(id state.HostID, address string) error
	AddVoter(id state.HostID, address string) error
	DemoteVoter(id state.HostID) error
	RemoveMember(id state.HostID) error
	TransferLeadership() error
}

// memberAction is one change the members loop makes.
type memberAction string

const (
	noMemberAction  memberAction = ""
	addNonvoter     memberAction = "add as a non-voter"
	addVoter        memberAction = "make a voter"
	demoteVoter     memberAction = "take the vote of"
	removeMember    memberAction = "remove from Raft"
	handOverLead    memberAction = "hand the lead over, to be removed"
	makeActive      memberAction = "make active"
	recordVoterRole memberAction = "record the Raft role of"
	removeLeaving   memberAction = "remove from the cluster once empty"
)

// memberStep is the next change: an action on a host.
type memberStep struct {
	action memberAction
	host   state.HostID
	// voter is the role that recordVoterRole records
	voter bool
}

// RunMembers keeps Raft's members and the hosts' roles as the state wants
// them while this agent leads, until ctx ends. It makes one change at a
// time (nextMemberStep):
//   - Every host that is not removed is a member; a joining host starts as
//     a non-voter and becomes active once its agent has reported.
//   - Of the control-plane hosts in service, 1, 3 or 5 vote (voterCount):
//     first those that voted already, then those with the lowest IDs.
//   - A removed host leaves Raft once another host has taken its vote.
//   - The state records each host's vote as Raft has it (set-host-voter).
//
// raftAddress is a host's Raft address.
func RunMembers(ctx context.Context, cluster Cluster, members Members, propose Propose,
	raftAddress func(state.HostID) string, self state.HostID, logger *slog.Logger,
) {
	for {
		changed := cluster.Changed()
		wait := interval
		if cluster.IsLeader() {
			step, err := tendMembers(ctx, cluster, members, propose, raftAddress, self)
			switch {
			case err != nil:
				logger.Warn("keep Raft's members", "error", err)
			case step.action != noMemberAction:
				logger.Info("Raft's members change", "action", string(step.action), "host", step.host)
				wait = 0 // the next change may follow at once
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-changed:
		case <-time.After(wait):
		}
	}
}

// tendMembers makes the next change, if there is one, and returns it.
func tendMembers(ctx context.Context, cluster Cluster, members Members, propose Propose,
	raftAddress func(state.HostID) string, self state.HostID,
) (memberStep, error) {
	copied := cluster.SharedCopy()
	configuration, err := members.Members()
	if err != nil {
		return memberStep{}, err
	}
	step := nextMemberStep(copied, configuration, self)
	switch step.action {
	case noMemberAction:
	case addNonvoter:
		err = members.AddNonvoter(step.host, raftAddress(step.host))
	case addVoter:
		err = members.AddVoter(step.host, raftAddress(step.host))
	case demoteVoter:
		err = members.DemoteVoter(step.host)
	case removeMember:
		err = members.RemoveMember(step.host)
	case handOverLead:
		err = members.TransferLeadership()
	case makeActive:
		err = movePhase(ctx, propose, copied.Hosts[step.host], state.Active)
	case removeLeaving:
		err = movePhase(ctx, propose, copied.Hosts[step.host], state.Removed)
	case recordVoterRole:
		_, err = commit(ctx, propose, state.KindSetHostVoter, state.SetHostVoter{Host: step.host, Voter: step.voter})
	}
	if err != nil {
		return step, fmt.Errorf("%s host %d: %w", step.action, step.host, err)
	}
	return step, nil
}

// movePhase moves the host from the phase that the loop saw to phase.
func movePhase(ctx context.Context, propose Propose, host *state.Host, phase state.HostPhase) error {
	_, err := commit(ctx, propose, state.KindSetHostPhase, state.SetHostPhase{Host: host.ID, Phase: phase, From: host.Phase})
	return err
}

// voterCount returns how many of n control-plane hosts vote. Below three
// hosts, one votes, because two voters would stop when either is lost. So
// the second control-plane host waits for its vote until a third one comes.
// From six hosts on, five vote.
func voterCount(n int) int {
	switch {
	case n >= 6:
		return 5
	case n >= state.MinHAHosts:
		return 3
	default:
		return 1
	}
}

// nextMemberStep decides the next change. configuration is Raft's
// configuration, and self is the leader. The order matters:
//  1. A removed leader hands over its lead first. The API normally does
//     this before the removal; this is the fallback. The leader's revoked
//     certificate stops new connections, but Raft's open connections stay
//     up, and so would its lead.
//  2. Hosts that are not members join Raft. A voter that is joining or has
//     no control plane loses its vote.
//  3. A leaving host (Host.Leaving) with nothing left on it is removed. If
//     it leads, it hands over its lead first, and the next leader removes
//     it. Then a joining host whose agent has reported becomes active.
//  4. Votes move before a removed voter leaves, so the cluster never has
//     fewer voters along the way than at the end.
//  5. Removed hosts leave Raft.
//  6. The state records Raft's votes.
func nextMemberStep(current *state.State, configuration map[state.HostID]bool, self state.HostID) memberStep {
	ids := slices.Sorted(maps.Keys(current.Hosts))
	isMember := func(id state.HostID) bool {
		_, member := configuration[id]
		return member
	}
	if leader, ok := current.Hosts[self]; ok && leader.Phase == state.Removed && isMember(self) {
		return memberStep{action: handOverLead, host: self}
	}
	for _, id := range ids {
		host := current.Hosts[id]
		switch {
		case host.Phase != state.Removed && !isMember(id):
			return memberStep{action: addNonvoter, host: id}
		// a host that took the ID of a removed host before that host left
		// Raft inherits its vote. It gets no vote while it joins, and none
		// at all without the control plane.
		case (host.Phase == state.Joining || !host.Roles.Has(hostrole.ControlPlane)) && configuration[id] && id != self:
			return memberStep{action: demoteVoter, host: id}
		}
	}
	for _, id := range ids {
		// a leaving host is removed once nothing is left on it. If it
		// leads, it hands over its lead first, and the next leader removes
		// it.
		if current.Hosts[id].Leaving && current.CheckRemoval(id) == nil {
			if id == self {
				return memberStep{action: handOverLead, host: id}
			}
			return memberStep{action: removeLeaving, host: id}
		}
	}
	for _, id := range ids {
		// its agent runs: it has reported its version
		if host := current.Hosts[id]; host.Phase == state.Joining && host.Version != "" {
			return memberStep{action: makeActive, host: id}
		}
	}
	if step := nextVoter(current, configuration, ids, self); step.action != noMemberAction {
		return step
	}
	for _, id := range ids {
		if current.Hosts[id].Phase == state.Removed && isMember(id) {
			return memberStep{action: removeMember, host: id}
		}
	}
	for _, id := range ids {
		host := current.Hosts[id]
		if voter := configuration[id]; host.Phase != state.Removed && host.Voter != voter {
			return memberStep{action: recordVoterRole, host: id, voter: voter}
		}
	}
	return memberStep{}
}

// nextVoter promotes or demotes one host, so that voterCount of the
// eligible hosts vote. Eligible are the control-plane hosts that are in
// service, or out of service but expected back (see votes). A host that
// voted keeps its vote while it may, and the leader never gives up its own.
// Only a host in service gets a new vote, because a host that cannot answer
// would count toward a quorum that it does not help to reach.
//
// A failed or fenced voter keeps its vote. Taking it away would shrink the
// cluster below what it was built for. With three hosts, the leader would
// end up as the only voter, and the cluster would stop at the next failure.
func nextVoter(current *state.State, configuration map[state.HostID]bool, ids []state.HostID,
	self state.HostID,
) memberStep {
	var eligible, voting []state.HostID
	for _, id := range ids {
		if host := current.Hosts[id]; host.Roles.Has(hostrole.ControlPlane) && votes(host.Phase) {
			eligible = append(eligible, id)
			if configuration[id] {
				voting = append(voting, id)
			}
		}
	}
	want := voterCount(len(eligible))
	switch {
	case len(voting) < want:
		for _, id := range eligible {
			if phase := current.Hosts[id].Phase; !configuration[id] && state.InService(phase) {
				return memberStep{action: addVoter, host: id}
			}
		}
	case len(voting) > want:
		for _, id := range slices.Backward(voting) {
			if id != self {
				return memberStep{action: demoteVoter, host: id}
			}
		}
	}
	return memberStep{}
}

// votes reports whether a control-plane host in this phase counts toward
// the voters: it is in service, or it was and should come back after a
// failure.
func votes(phase state.HostPhase) bool {
	switch phase {
	case state.Active, state.Draining, state.Failed, state.Fencing, state.Fenced, state.Recovering, state.Disabled:
		return true
	}
	return false
}
