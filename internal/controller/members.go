package controller

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/webcodingcat/clowder/internal/state"
)

// Members is Raft's configuration as the leader changes it
// (raftnode.Node). Raft changes its members one at a time.
type Members interface {
	// Members are the hosts in Raft, each with whether it votes.
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
	makePassive     memberAction = "make passive"
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
// time:
//   - Every host that is not removed is a member. A joining host becomes a
//     member as a non-voter.
//   - A joining host that has reported (its agent runs) becomes passive
//     while it is the second host of a cluster that is not highly available
//     yet; otherwise it becomes active. A passive host becomes active once
//     there are three hosts.
//   - Of the active hosts, 1, 3 or 5 vote (voterCount): first those that
//     voted already, then those with the lowest IDs.
//   - A removed host leaves Raft once another host has taken its vote. A
//     removed leader hands over its lead first.
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
	copied, err := cluster.SharedCopy()
	if err != nil {
		return memberStep{}, err
	}
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
	case makePassive:
		err = movePhase(ctx, propose, copied.Hosts[step.host], state.Passive)
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

// movePhase moves the host to phase, from the phase the loop saw.
func movePhase(ctx context.Context, propose Propose, host *state.Host, phase state.HostPhase) error {
	_, err := commit(ctx, propose, state.KindSetHostPhase, state.SetHostPhase{Host: host.ID, Phase: phase, From: host.Phase})
	return err
}

// voterCount is how many of n active hosts vote. Below three hosts, one
// votes, because two voters would stop when either is lost. From six
// hosts on, five vote.
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
//  1. A leader that is removed hands over its lead, before anything else.
//     The API hands over the lead before it removes the host; this step is
//     the fallback for a removal that the API did not see coming. The
//     removal revoked the leader's certificate, so the others accept no new
//     connection from it. But Raft's open connections stay up, and so
//     would its lead.
//  2. Hosts that are not members join Raft, and a joining or passive host
//     that is a voter loses its vote.
//  3. A leaving host (Host.Leaving) with nothing left on it is removed; if
//     it leads, another leader removes it. Then joining and passive hosts
//     move on to their next phase.
//  4. Votes go where they belong before a removed voter leaves, so that
//     the cluster never has fewer voters along the way than at the end.
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
		phase := current.Hosts[id].Phase
		switch {
		case phase != state.Removed && !isMember(id):
			return memberStep{action: addNonvoter, host: id}
		// a host that took the ID of a removed host before that host left
		// Raft inherits its vote; it gets none until it runs stacks
		case (phase == state.Joining || phase == state.Passive) && configuration[id] && id != self:
			return memberStep{action: demoteVoter, host: id}
		}
	}
	for _, id := range ids {
		// a leaving host is removed once nothing is left on it. A leader
		// hands over its lead first, and the next leader removes it.
		if current.Hosts[id].Leaving && current.CheckRemoval(id) == nil {
			if id == self {
				return memberStep{action: handOverLead, host: id}
			}
			return memberStep{action: removeLeaving, host: id}
		}
	}
	if step := nextPhase(current, ids); step.action != noMemberAction {
		return step
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

// nextPhase moves a joining host that has reported, or a passive host, on
// to its next phase.
func nextPhase(current *state.State, ids []state.HostID) memberStep {
	// settled are the hosts that run stacks or wait to run them (passive);
	// joining hosts do not count. With one active host and two joining
	// hosts, the first to report becomes passive (settled is 1), the second
	// becomes active (settled is 2), and then the passive one becomes
	// active (settled is 3).
	settled := 0
	for _, host := range current.Hosts {
		if host.Phase == state.Active || host.Phase == state.Draining || host.Phase == state.Passive {
			settled++
		}
	}
	for _, id := range ids {
		host := current.Hosts[id]
		switch {
		case host.Phase == state.Joining && host.Version != "" && !current.HA && settled == 1:
			return memberStep{action: makePassive, host: id}
		case host.Phase == state.Joining && host.Version != "":
			return memberStep{action: makeActive, host: id}
		case host.Phase == state.Passive && (current.HA || settled >= state.MinHAHosts):
			return memberStep{action: makeActive, host: id}
		}
	}
	return memberStep{}
}

// nextVoter promotes or demotes one host, so that voterCount of the
// eligible hosts vote. Eligible hosts run stacks, or ran them and are to
// come back (votes). A host that voted keeps its vote while it may, and the
// leader never gives up its own. Only an active or draining host gets a new
// vote: a host that cannot answer would count toward a quorum that it does
// not help to reach.
//
// A failed or fenced voter keeps its vote. Taking it away would shrink the
// cluster below what it was built for. With three hosts, the vote would go
// to a live host instead, leaving the leader as the only voter that answers,
// and the cluster would stop at the next failure.
func nextVoter(current *state.State, configuration map[state.HostID]bool, ids []state.HostID,
	self state.HostID,
) memberStep {
	var eligible, voting []state.HostID
	for _, id := range ids {
		if votes(current.Hosts[id].Phase) {
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
			if phase := current.Hosts[id].Phase; !configuration[id] && state.RunsStacks(phase) {
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

// votes tells whether a host in this phase counts for the voters: it runs
// stacks, or it ran them and is to come back after a failure.
func votes(phase state.HostPhase) bool {
	switch phase {
	case state.Active, state.Draining, state.Failed, state.Fencing, state.Fenced, state.Recovering, state.Disabled:
		return true
	}
	return false
}
