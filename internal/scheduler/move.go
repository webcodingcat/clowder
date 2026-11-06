package scheduler

import "github.com/webcodingcat/clowder/internal/state"

// MoveVolumes returns the records of a moving stack's volumes, each with a
// copy on the host that the stack moves to. The first step of the move
// writes them; the move's operation holds their locks. If the volumes
// cannot go there, it returns the reason instead.
func MoveVolumes(current *state.State, stackID string, to state.HostID) ([]state.Volume, string) {
	stack, ok := current.Stacks[stackID]
	if !ok {
		return nil, "the stack is gone"
	}
	planner := newPlanner(current)
	if move := current.MoveOf(stackID); move != nil {
		planner.operation = move.ID
	}
	volumes, copies, reason := planner.volumesOn(stack, to)
	return append(volumes, copies...), reason
}
