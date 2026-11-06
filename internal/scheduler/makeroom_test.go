package scheduler

import (
	"strings"
	"testing"

	"github.com/webcodingcat/clowder/internal/hostrole"
	"github.com/webcodingcat/clowder/internal/state"
)

// twoRoomHosts are roomCluster's host 0 and host 1, each offering 1000M,
// with blog (priority 200, 600M) running on host 0 and nothing on host 1.
func twoRoomHosts() *state.State {
	current := roomCluster(1000)
	current.Hosts[1] = &state.Host{
		ID: 1, Name: "node1", Roles: hostrole.DefaultInit, Phase: state.Active, Capacity: current.Hosts[0].Capacity,
	}
	running(current, "blog", map[string]state.Service{"app": service(200, 600)})
	return current
}

func TestAStackOfLowerPriorityMovesToMakeRoomBeforeAnyServiceIsPushedOut(t *testing.T) {
	current := twoRoomHosts()
	// new needs 700M on host 0, where its data is: host 1 has room for
	// blog but would need a copy of new's data
	pending(current, map[string]state.Service{"web": service(800, 700)})
	current.Hosts[1].Phase = state.Draining // host 1 cannot take new
	if decisions := Plan(current, planTime); len(decisions.Moves) != 0 {
		t.Fatalf("a move to a host that takes no stacks: %+v", decisions.Moves)
	}
	current.Hosts[1].Phase = state.Active
	// new fits whole on host 1 at once: no move needed
	if decisions := Plan(current, planTime); len(decisions.Moves) != 0 || len(decisions.Placements) != 1 ||
		decisions.Placements[0].Host != 1 {
		t.Fatalf("decisions %+v", decisions)
	}
	// with host 1 at 500M free, new fits nowhere whole: blog moves there
	running(current, "files", map[string]state.Service{"app": service(900, 500)})
	current.Stacks["default/files"].Host = &[]state.HostID{1}[0]
	current.Stacks["default/blog"].Services["app"] = service(200, 400)
	decisions := Plan(current, planTime)
	if len(decisions.Moves) != 1 || decisions.Moves[0].Stack != "default/blog" || decisions.Moves[0].To != 1 {
		t.Fatalf("moves %+v, placements %+v, waiting %+v", decisions.Moves, decisions.Placements, decisions.Waiting)
	}
	if len(decisions.Waiting) != 1 || !strings.Contains(decisions.Waiting[0].Reason, "waits for default/blog to move") {
		t.Errorf("waiting %+v", decisions.Waiting)
	}
	if len(decisions.Placements) != 0 || len(decisions.PushOuts) != 0 {
		t.Errorf("a service is pushed out as well: %+v", decisions)
	}
}

func TestAStackMovedWithinTheCooldownStays(t *testing.T) {
	current := twoRoomHosts()
	running(current, "files", map[string]state.Service{"app": service(900, 500)})
	current.Stacks["default/files"].Host = &[]state.HostID{1}[0]
	current.Stacks["default/blog"].Services["app"] = service(200, 400)
	current.Stacks["default/blog"].LastMove = planTime.Add(-MoveCooldown / 2)
	pending(current, map[string]state.Service{"web": service(800, 700)})
	decisions := Plan(current, planTime)
	if len(decisions.Moves) != 0 {
		t.Errorf("a stack in its cooldown moved: %+v", decisions.Moves)
	}
	// it pushes blog out instead
	if len(decisions.Placements) != 1 || decisions.Placements[0].Host != 0 {
		t.Errorf("placements %+v", decisions.Placements)
	}
}

func TestAStackWaitsForTheMovesThatMakeItsRoomInsteadOfSendingMore(t *testing.T) {
	current := twoRoomHosts()
	running(current, "files", map[string]state.Service{"app": service(900, 500)})
	current.Stacks["default/files"].Host = &[]state.HostID{1}[0]
	current.Stacks["default/blog"].Services["app"] = service(200, 300)
	running(current, "notes", map[string]state.Service{"app": service(100, 100)})
	pending(current, map[string]state.Service{"web": service(800, 700)})
	// blog moves off host 0 already and has not stopped there
	zero := state.HostID(0)
	current.Operations = map[uint64]*state.Operation{7: {
		ID: 7, Kind: state.OperationMoveStack, Step: state.MoveCopies, State: state.OpRunning,
		Move: &state.StackMove{Stack: "default/blog", From: &zero, To: 1},
	}}
	current.Locks = map[string]uint64{"stack/default/blog": 7}
	decisions := Plan(current, planTime)
	if len(decisions.Moves) != 0 {
		t.Errorf("more stacks sent away: %+v", decisions.Moves)
	}
	if len(decisions.Waiting) != 1 || !strings.Contains(decisions.Waiting[0].Reason, "waits for default/blog to move off node0") {
		t.Errorf("waiting %+v", decisions.Waiting)
	}
}
