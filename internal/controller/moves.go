package controller

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/scheduler"
	"github.com/webcodingcat/clowder/internal/state"
	"github.com/webcodingcat/clowder/internal/units"
)

// Progress asks a host how far its copies are from their peers, by volume
// ID, in bytes (volumes.Reconciler.Progress, over the agent's API).
type Progress func(ctx context.Context, host state.HostID) (map[string]int64, error)

// progressWindow is how much longer a resync that still makes progress
// gets once its deadline passed, until the next look.
const progressWindow = 5 * time.Minute

// mover drives the move-stack operations while this agent leads; what it
// keeps in memory a new leader starts afresh.
type mover struct {
	cluster  Cluster
	propose  Propose
	progress Progress
	logger   *slog.Logger
	// looks are the last looks at each late move's resync,
	// by operation ID
	looks map[uint64]look
}

// RunMoves drives the move-stack operations while this agent leads, until
// ctx ends: the leader's steps, and the deadlines of every step. The
// placement starts them (scheduler.Move); the hosts run their own steps
// (agent/moves.go).
func RunMoves(ctx context.Context, cluster Cluster, propose Propose, progress Progress, logger *slog.Logger) {
	m := &mover{cluster: cluster, propose: propose, progress: progress, logger: logger, looks: map[uint64]look{}}
	for {
		changed := cluster.Changed()
		wait := interval
		if cluster.IsLeader() {
			if soonest := m.pass(ctx); soonest > 0 {
				wait = min(wait, soonest)
			}
		} else {
			// a leader that gets the lead back looks afresh
			clear(m.looks)
		}
		select {
		case <-ctx.Done():
			return
		case <-changed:
		case <-time.After(wait):
		}
	}
}

// pass looks at each move once and tells how soon the next deadline is.
func (m *mover) pass(ctx context.Context) time.Duration {
	copied, err := m.cluster.SharedCopy()
	if err != nil {
		m.logger.Error("move the stacks", "error", err)
		return 0
	}
	now := time.Now()
	soonest := time.Duration(0)
	for _, id := range slices.Sorted(maps.Keys(copied.Operations)) {
		operation := copied.Operations[id]
		if operation.Move == nil || (operation.State != state.OpRunning && operation.State != state.OpRollingBack) {
			delete(m.looks, id)
			continue
		}
		if err := m.step(ctx, copied, operation, now); err != nil {
			m.logger.Error("move a stack", "operation", id, "stack", operation.Move.Stack, "error", err)
		}
		if left := operation.Deadline.Sub(now); left > 0 && (soonest == 0 || left < soonest) {
			soonest = left
		}
	}
	return soonest
}

// step takes a move on: it rolls back what is late before its point of no
// return, ends what is late after it, and runs the leader's steps. A move
// whose host went down ends at once, the same way (abandon).
func (m *mover) step(ctx context.Context, copied *state.State, operation *state.Operation, now time.Time) error {
	move := operation.Move
	stack, ok := copied.Stacks[move.Stack]
	down, isDown := downHost(copied, move)
	switch {
	case operation.State == state.OpRollingBack:
		// nothing to undo: the stack runs on its old host again (its status
		// shows running meanwhile, as after a restart), and the placement
		// gives up the copies the move left once the locks are free
		return m.finish(ctx, operation, true, "")
	case !ok:
		return m.finish(ctx, operation, true, "the stack is gone")
	case isDown && operation.Step < state.MoveHost:
		return m.abandon(ctx, operation, fmt.Sprintf("host %s is %s", down.Name, down.Phase))
	case isDown && down.ID == move.To:
		// the stack is the new host's: the failover takes it from there
		return m.finish(ctx, operation, true, fmt.Sprintf("host %s is %s", down.Name, down.Phase))
	case now.After(operation.Deadline):
		return m.late(ctx, copied, operation, now)
	case operation.Owner != nil:
		return nil // a host runs the step
	}
	switch operation.Step {
	case state.MoveStart:
		return m.placeCopies(ctx, copied, operation, now)
	case state.MoveStopped:
		return m.setHost(ctx, copied, operation)
	case state.MoveHost:
		if stack.Host != nil && *stack.Host == move.To && stack.Status == state.StackRunning {
			m.logger.Info("a stack moved", "stack", move.Stack, "to", copied.Hosts[move.To].Name)
			return m.finish(ctx, operation, false, "")
		}
	}
	return nil
}

// placeCopies runs the first step: a copy of each volume on the new host,
// then the new host has the step that waits for them. Its deadline gives
// the resync time for all of the stack's volumes at the resync rate,
// twice over (syncTime).
func (m *mover) placeCopies(ctx context.Context, copied *state.State, operation *state.Operation, now time.Time) error {
	move := operation.Move
	volumes, reason := scheduler.MoveVolumes(copied, move.Stack, move.To)
	if reason != "" {
		return m.rollBack(ctx, operation, reason)
	}
	for _, volume := range volumes {
		if _, err := commit(ctx, m.propose, state.KindPutVolume, state.PutVolume{
			Volume: volume, PlannedAt: copied.Index, Operation: operation.ID,
		}); err != nil {
			return fmt.Errorf("place a copy of %s on the new host: %w", volume.ID(), err)
		}
	}
	to := move.To
	_, err := commit(ctx, m.propose, state.KindAdvanceOperation, state.AdvanceOperation{
		ID: operation.ID, FromStep: state.MoveStart, Step: state.MoveCopies, Owner: &to,
		Deadline: now.Add(syncTime(copied, move.Stack)),
	})
	return err
}

// syncTime is how long the resync of a stack's volumes may take: StepTime,
// and twice what all of them take at the resync rate.
func syncTime(copied *state.State, stackID string) time.Duration {
	rate, err := units.ParseSize(copied.Config.Spec.WithDefaults().Replication.ResyncRate)
	if err != nil || rate <= 0 {
		return state.StepTime
	}
	stack := copied.Stacks[stackID]
	total := int64(0)
	for _, name := range stack.Spec.VolumeNames() {
		if volume, ok := copied.Volumes[names.ID(stack.Namespace, name)]; ok && volume.Spec.Replicas > 0 {
			total += int64(volume.Spec.Size)
		}
	}
	// in seconds first: total in bytes times a second in nanoseconds would
	// overflow from a few GiB on
	return state.StepTime + time.Duration(2*total/rate+1)*time.Second
}

// setHost runs the step after the old host stopped the stack: a volume of
// replicas 0 gets a new, empty LV on the new host (a new data generation),
// and the stack the new host.
func (m *mover) setHost(ctx context.Context, copied *state.State, operation *state.Operation) error {
	move := operation.Move
	stack := copied.Stacks[move.Stack]
	for _, name := range stack.Spec.VolumeNames() {
		volume, ok := copied.Volumes[names.ID(stack.Namespace, name)]
		if !ok || volume.Spec.Replicas > 0 || volume.Status == state.VolumePending || len(volume.Copies) == 0 ||
			slices.ContainsFunc(volume.Copies, func(c state.VolumeCopy) bool { return c.Host == move.To }) {
			continue
		}
		fresh := *volume
		fresh.DataGeneration++
		fresh.Copies = []state.VolumeCopy{{Host: move.To, Class: volume.Copies[0].Class}}
		if _, err := commit(ctx, m.propose, state.KindPutVolume, state.PutVolume{
			Volume: fresh, PlannedAt: copied.Index, Operation: operation.ID,
		}); err != nil {
			return fmt.Errorf("give %s a new LV on the new host: %w", volume.ID(), err)
		}
	}
	to := move.To
	if _, err := commit(ctx, m.propose, state.KindSetStackStatus, state.SetStackStatus{
		ID: move.Stack, Status: state.StackStarting, Host: &to, PlannedAt: copied.Index, Operation: operation.ID,
	}); err != nil {
		return fmt.Errorf("give the stack its new host: %w", err)
	}
	_, err := commit(ctx, m.propose, state.KindAdvanceOperation, state.AdvanceOperation{
		ID: operation.ID, FromStep: state.MoveStopped, Step: state.MoveHost,
	})
	return err
}

// late handles a step past its deadline. The step that waits for the new
// host's copies gets more time while it gets on (lateResync); any other
// step before the point of no return rolls back. After it, the move ends
// as failed: the stack stays where the move got it, and the placement
// takes it from there.
func (m *mover) late(ctx context.Context, copied *state.State, operation *state.Operation, now time.Time) error {
	if operation.Step >= state.MoveStopped {
		return m.finish(ctx, operation, true, fmt.Sprintf("it did not get past %s in time", state.MoveStepName(operation.Step)))
	}
	if operation.Step != state.MoveCopies {
		return m.rollBack(ctx, operation, fmt.Sprintf("it did not get past %s in time", state.MoveStepName(operation.Step)))
	}
	if reason := m.lateResync(ctx, copied, operation); reason != "" {
		return m.rollBack(ctx, operation, reason)
	}
	m.logger.Info("a move's resync is late but gets on; it gets more time", "operation", operation.ID,
		"stack", operation.Move.Stack)
	_, err := commit(ctx, m.propose, state.KindExtendOperation, state.ExtendOperation{
		ID: operation.ID, Step: operation.Step, Deadline: now.Add(progressWindow),
	})
	return err
}

// lateResync looks at a move whose copies on the new host are late, and
// says why it should roll back; empty means it gets one more window
// (progressWindow). Copies the state knows as synced mean the images are
// still coming: they get one window. Otherwise it asks the new host what
// is left of the resync of the stack's volumes and compares with the last
// look:
//   - the first look has nothing to compare with, and gets a window, also
//     when the host does not answer (a network hiccup), and so does the
//     first answer after one;
//   - less left than at the last look gets a window; the same or more, or
//     no answer a second time, rolls back.
//
// The looks are in the leader's memory: a new leader looks afresh, and so
// gives at most one window more per change of leader.
func (m *mover) lateResync(ctx context.Context, copied *state.State, operation *state.Operation) string {
	last, looked := m.looks[operation.ID]
	if copiesSynced(copied, operation.Move) {
		m.looks[operation.ID] = look{synced: true}
		if looked && last.synced {
			return "the stack's images did not come to the new host in time"
		}
		return ""
	}
	left, err := m.progress(ctx, operation.Move.To)
	if err != nil {
		m.logger.Warn("ask the new host how far the resync is", "operation", operation.ID, "error", err)
		m.looks[operation.ID] = look{}
		if looked && !last.answered {
			return "the new host did not tell how far the resync of its copies is"
		}
		return ""
	}
	stack := copied.Stacks[operation.Move.Stack]
	total := int64(0)
	for _, name := range stack.Spec.VolumeNames() {
		total += left[names.ID(stack.Namespace, name)]
	}
	m.looks[operation.ID] = look{answered: true, left: total}
	if !looked || !last.answered || total < last.left {
		return ""
	}
	return "the resync of its copies on the new host did not get on in time"
}

// look is what the leader saw at its last look at a late move's resync.
type look struct {
	// answered: the new host told what is left of the resync (left)
	answered bool
	left     int64
	// synced: the state knew the copies as synced, only the images missed
	synced bool
}

// copiesSynced tells whether every copy a move needs on the new host is
// synced, as the hosts told the state; a volume that never held data has
// nothing to sync.
func copiesSynced(copied *state.State, move *state.StackMove) bool {
	stack := copied.Stacks[move.Stack]
	for _, name := range stack.Spec.VolumeNames() {
		volume, ok := copied.Volumes[names.ID(stack.Namespace, name)]
		if !ok || volume.Spec.Replicas == 0 || !volume.Initialized {
			continue
		}
		if !slices.ContainsFunc(volume.Copies, func(c state.VolumeCopy) bool { return c.Host == move.To && c.Synced }) {
			return false
		}
	}
	return true
}

// rollBack turns a move back: the stack keeps or gets back its old host.
func (m *mover) rollBack(ctx context.Context, operation *state.Operation, reason string) error {
	m.logger.Warn("a move rolls back", "operation", operation.ID, "stack", operation.Move.Stack, "reason", reason)
	_, err := commit(ctx, m.propose, state.KindRollBackOperation, state.RollBackOperation{
		ID: operation.ID, FromStep: operation.Step, Error: reason,
	})
	return err
}

// finish ends a move; one that failed says why in the log.
func (m *mover) finish(ctx context.Context, operation *state.Operation, failed bool, reason string) error {
	if failed {
		m.logger.Warn("a move failed", "operation", operation.ID, "stack", operation.Move.Stack,
			"reason", cmp.Or(reason, operation.Error))
	}
	delete(m.looks, operation.ID)
	_, err := commit(ctx, m.propose, state.KindFinishOperation, state.FinishOperation{
		ID: operation.ID, Failed: failed, Error: reason,
	})
	return err
}

// downHost is a host of the move that went down: no longer active or
// draining, as when it failed. The move cannot wait for it to come back.
func downHost(copied *state.State, move *state.StackMove) (*state.Host, bool) {
	ids := []state.HostID{move.To}
	if move.From != nil {
		ids = append(ids, *move.From)
	}
	for _, id := range ids {
		if host := copied.Hosts[id]; !state.RunsStacks(host.Phase) {
			return host, true
		}
	}
	return nil, false
}

// abandon ends a move whose host went down before the new host got the
// stack: before the point of no return it rolls back, after it the move
// fails. Either way the stack keeps its old host; if that is the one that
// is down, the failover takes the stack once the host is fenced.
func (m *mover) abandon(ctx context.Context, operation *state.Operation, reason string) error {
	if operation.Step >= state.MoveStopped {
		return m.finish(ctx, operation, true, reason)
	}
	return m.rollBack(ctx, operation, reason)
}
