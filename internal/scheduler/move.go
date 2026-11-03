package scheduler

import "github.com/webcodingcat/clowder/internal/state"

// MoveVolumes are the records of a moving stack's volumes with a copy on
// the host it moves to, for the first step of its move (the operation
// that holds their locks); a reason says why they cannot go there.
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
