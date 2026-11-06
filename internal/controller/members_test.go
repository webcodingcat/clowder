package controller

import (
	"testing"

	"github.com/webcodingcat/clowder/internal/hostrole"
	"github.com/webcodingcat/clowder/internal/state"
)

// hostsIn is a state with hosts in the given phases, by ID, each with every
// role; a host that is not joining has reported its version, and host 0
// votes, as the bootstrap made it.
func hostsIn(phases ...state.HostPhase) *state.State {
	current := &state.State{Hosts: map[state.HostID]*state.Host{}}
	for index, phase := range phases {
		host := &state.Host{
			ID: state.HostID(index), Roles: hostrole.DefaultInit, Phase: phase, Version: "0.2.0", Voter: index == 0,
		}
		if phase == state.Joining {
			host.Version = ""
		}
		current.Hosts[host.ID] = host
	}
	return current
}

// voters is Raft's configuration: every host a member, the given ones
// voters.
func voters(hosts int, voting ...state.HostID) map[state.HostID]bool {
	configuration := map[state.HostID]bool{}
	for id := range state.HostID(hosts) {
		configuration[id] = false
	}
	for _, id := range voting {
		configuration[id] = true
	}
	return configuration
}

func TestTheMembersFollowTheHostsOneStepAtATime(t *testing.T) {
	const active, joining, removed = state.Active, state.Joining, state.Removed
	reported := func(current *state.State, id state.HostID) *state.State {
		current.Hosts[id].Version = "0.2.0"
		return current
	}
	ha := func(current *state.State) *state.State {
		current.HA = true
		return current
	}
	for _, test := range []struct {
		name          string
		current       *state.State
		configuration map[state.HostID]bool
		want          memberStep
	}{
		{"one host: nothing", hostsIn(active), voters(1, 0), memberStep{}},
		{"a joining host becomes a member", hostsIn(active, joining), voters(1, 0), memberStep{action: addNonvoter, host: 1}},
		{"it waits for its agent", hostsIn(active, joining), voters(2, 0), memberStep{}},
		{
			"the second host is active once its agent runs", reported(hostsIn(active, joining), 1), voters(2, 0),
			memberStep{action: makeActive, host: 1},
		},
		{"of two control-plane hosts, one votes", hostsIn(active, active), voters(2, 0), memberStep{}},
		{"three active hosts get three voters", hostsIn(active, active, active), voters(3, 0), memberStep{action: addVoter, host: 1}},
		{"one by one", hostsIn(active, active, active), voters(3, 0, 1), memberStep{action: addVoter, host: 2}},
		{
			"the state records Raft's voters", hostsIn(active, active, active), voters(3, 0, 1, 2),
			memberStep{action: recordVoterRole, host: 1, voter: true},
		},
		{
			"a host joining a highly available cluster is active",
			ha(reported(hostsIn(active, active, active, joining), 3)), voters(4, 0, 1, 2),
			memberStep{action: makeActive, host: 3},
		},
		{
			"the fourth active host does not vote", ha(hostsIn(active, active, active, active)), voters(4, 0, 1, 2),
			memberStep{action: recordVoterRole, host: 1, voter: true},
		},
		{
			"six active hosts get five voters", ha(hostsIn(active, active, active, active, active, active)),
			voters(6, 0, 1, 2),
			memberStep{action: addVoter, host: 3},
		},
		{
			"a removed host leaves Raft", ha(hostsIn(active, active, active, active, removed)), voters(5, 0, 1, 2, 4),
			memberStep{action: removeMember, host: 4},
		},
		{
			"then a non-voter takes its vote", ha(hostsIn(active, active, active, active, removed)), voters(4, 0, 1),
			memberStep{action: addVoter, host: 2},
		},
		{
			"a removed leader hands its lead over", ha(hostsIn(removed, active, active, active)), voters(4, 0, 1, 2),
			memberStep{action: handOverLead, host: 0},
		},
		{
			"a replacement votes before the removed voter leaves", ha(hostsIn(active, active, removed, active)),
			voters(4, 0, 1, 2),
			memberStep{action: addVoter, host: 3},
		},
		{
			"then the removed voter leaves", ha(hostsIn(active, active, removed, active)), voters(4, 0, 1, 2, 3),
			memberStep{action: removeMember, host: 2},
		},
		{
			"the leader keeps its vote", ha(hostsIn(active, active, active, removed, removed, removed)),
			voters(3, 0, 1, 2),
			memberStep{action: recordVoterRole, host: 1, voter: true},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := nextMemberStep(test.current, test.configuration, 0); got != test.want {
				t.Errorf("got %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestOnlyControlPlaneHostsVote(t *testing.T) {
	// workers makes the given hosts workers with ingress
	workers := func(current *state.State, ids ...state.HostID) *state.State {
		for _, id := range ids {
			current.Hosts[id].Roles = hostrole.DefaultJoin
		}
		return current
	}
	const active = state.Active
	for _, test := range []struct {
		name          string
		current       *state.State
		configuration map[state.HostID]bool
		want          memberStep
	}{
		{"workers get no vote", workers(hostsIn(active, active, active), 1, 2), voters(3, 0), memberStep{}},
		{
			"a worker that votes loses its vote", workers(hostsIn(active, active, active), 1, 2), voters(3, 0, 1),
			memberStep{action: demoteVoter, host: 1},
		},
		{
			"three control-plane hosts among workers get three voters",
			workers(hostsIn(active, active, active, active), 2), voters(4, 0, 1),
			memberStep{action: addVoter, host: 3},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := nextMemberStep(test.current, test.configuration, 0); got != test.want {
				t.Errorf("got %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestTooManyVotersGiveUpTheirVotesButTheLeaders(t *testing.T) {
	current := hostsIn(state.Active, state.Active, state.Active, state.Active)
	current.HA = true
	// four voters for four active hosts, which have three: the highest ID
	// that is not the leader gives up its vote
	configuration := voters(4, 0, 1, 2, 3)
	if got := nextMemberStep(current, configuration, 3); got != (memberStep{action: demoteVoter, host: 2}) {
		t.Errorf("got %+v", got)
	}
	// a host that took a removed host's ID before it left Raft found its
	// vote, and loses it before it runs
	current = hostsIn(state.Active, state.Active, state.Active, state.Joining)
	current.HA = true
	if got := nextMemberStep(current, voters(4, 0, 1, 2, 3), 0); got != (memberStep{action: demoteVoter, host: 3}) {
		t.Errorf("a joining voter: got %+v", got)
	}
	if voterCount(1) != 1 || voterCount(2) != 1 || voterCount(5) != 3 || voterCount(32) != 5 {
		t.Error("voterCount")
	}
}

func TestAHostThatLeavesGoesOnceEmptyAndHandsItsLeadOverFirst(t *testing.T) {
	current := hostsIn(state.Active, state.Active, state.Active, state.Draining)
	current.Hosts[3].Leaving = true
	if got := nextMemberStep(current, voters(4, 0, 1, 2), 0); got != (memberStep{action: removeLeaving, host: 3}) {
		t.Errorf("an empty host that leaves: %+v", got)
	}
	if got := nextMemberStep(current, voters(4, 0, 1, 2), 3); got != (memberStep{action: handOverLead, host: 3}) {
		t.Errorf("a leader that leaves: %+v", got)
	}
}

func TestAFailedVoterKeepsItsVoteAndADeadHostGetsNone(t *testing.T) {
	for _, phase := range []state.HostPhase{state.Failed, state.Fencing, state.Fenced, state.Recovering, state.Disabled} {
		// a failed voter among three: the live ones keep theirs too
		current := hostsIn(state.Active, state.Active, phase)
		if got := nextMemberStep(current, voters(3, 0, 1, 2), 0); got.action == demoteVoter {
			t.Errorf("%s: %+v", phase, got)
		}
		// a host that is down gets no vote, though one is missing
		if got := nextMemberStep(current, voters(3, 0, 1), 0); got.action == addVoter {
			t.Errorf("%s: %+v", phase, got)
		}
	}
}
