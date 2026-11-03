package agent

import (
	"context"
	"log/slog"
	"maps"
	"slices"

	"github.com/webcodingcat/clowder/internal/agent/loop"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/state"
)

// moveSteps are what the steps of a move that this host runs look at: the
// stack runner, the volume reconciler and the prefetcher do the work, as
// the state tells them (State.StopsForMove); the steps only record it done.
type moveSteps struct {
	cluster loop.Cluster
	self    state.HostID
	propose func(context.Context, state.Command) (uint64, error)
	// imagesReady tells whether Docker has a stack's images, by its ID
	imagesReady func(stackID string) bool
	// stopped tells whether a stack, by its project, has no containers here
	stopped func(project string) (bool, error)
	// released tells whether this host's copy, by its LV, is unmounted and
	// secondary
	released func(lv string) bool
	logger   *slog.Logger
}

// run records the steps of moves this host owns as they are done, at every
// change of the state and every interval, until ctx ends.
func (m moveSteps) run(ctx context.Context) {
	loop.Run(ctx, m.cluster, nil, func() { m.pass(ctx) })
}

// pass records each step this host owns that is done.
func (m moveSteps) pass(ctx context.Context) {
	copied, err := m.cluster.SharedCopy()
	if err != nil {
		m.logger.Error("read the state", "error", err)
		return
	}
	for _, id := range slices.Sorted(maps.Keys(copied.Operations)) {
		operation := copied.Operations[id]
		if operation.Move == nil || operation.State != state.OpRunning || operation.Owner == nil || *operation.Owner != m.self {
			continue
		}
		stack, ok := copied.Stacks[operation.Move.Stack]
		if !ok {
			continue // the leader ends the move
		}
		var advance *state.AdvanceOperation
		switch operation.Step {
		case state.MoveCopies:
			advance = m.synced(copied, operation, stack)
		case state.MoveSynced:
			advance = m.stoppedHere(copied, operation, stack)
		}
		if advance == nil {
			continue
		}
		if err := loop.CommitAsHost(ctx, m.propose, m.self, state.KindAdvanceOperation, *advance); err != nil && ctx.Err() == nil {
			m.logger.Error("record a step of a move", "operation", id, "stack", stack.ID(), "error", err)
		}
	}
}

// synced is the step on the new host: each volume's copy here holds the
// data (a volume that never held any has none to wait for), and Docker has
// the images. The old host stops the stack next; a stack without one goes
// on with the leader.
func (m moveSteps) synced(copied *state.State, operation *state.Operation, stack *state.Stack) *state.AdvanceOperation {
	for _, name := range stack.Spec.VolumeNames() {
		volume, ok := copied.Volumes[names.ID(stack.Namespace, name)]
		if !ok || volume.Spec.Replicas == 0 {
			continue
		}
		index := slices.IndexFunc(volume.Copies, func(c state.VolumeCopy) bool { return c.Host == m.self && !c.Tiebreaker })
		// a volume that never held data has nothing to sync
		if index < 0 || (volume.Initialized && !volume.Copies[index].Synced) {
			return nil
		}
	}
	if !m.imagesReady(stack.ID()) {
		return nil
	}
	// the old host stops the stack next; a stack that had none has nothing
	// to stop, so the leader gives it its host next
	advance := &state.AdvanceOperation{ID: operation.ID, FromStep: state.MoveCopies, Step: state.MoveSynced, Owner: operation.Move.From}
	if operation.Move.From == nil {
		advance.Step = state.MoveStopped
	}
	return advance
}

// stoppedHere is the step on the old host: the stack has no containers
// here any more, and each of its copies here is unmounted and secondary.
// The leader gives the stack its new host next.
func (m moveSteps) stoppedHere(copied *state.State, operation *state.Operation, stack *state.Stack) *state.AdvanceOperation {
	stopped, err := m.stopped(stack.Project())
	if err != nil {
		m.logger.Error("look whether a moving stack stopped", "stack", stack.ID(), "error", err)
		return nil
	}
	if !stopped {
		return nil
	}
	for _, name := range stack.Spec.VolumeNames() {
		id := names.ID(stack.Namespace, name)
		volume, ok := copied.Volumes[id]
		if !ok || !slices.ContainsFunc(volume.Copies, func(c state.VolumeCopy) bool { return c.Host == m.self }) {
			continue
		}
		if !m.released(names.VolumeName(id, volume.DataGeneration)) {
			return nil
		}
	}
	return &state.AdvanceOperation{ID: operation.ID, FromStep: state.MoveSynced, Step: state.MoveStopped}
}
