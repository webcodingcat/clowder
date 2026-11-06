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

// Progress asks a host how far its copies are behind their peers, in bytes
// by volume ID (volumes.Reconciler.Progress, through the agent's API).
type Progress func(ctx context.Context, host state.HostID) (map[string]int64, error)

// progressWindow is the extra time that a resync gets after its deadline
// has passed, as long as it still makes progress.
const progressWindow = 5 * time.Minute

// mover keeps its looks in memory only, so a new leader starts them afresh.
type mover struct {
	cluster  Cluster
	propose  Propose
	progress Progress
	logger   *slog.Logger
	// looks holds the leader's last look at each late move's resync, by
	// operation ID
	looks map[uint64]look
}

// RunMoves carries out the move-stack operations while this agent leads,
// until ctx ends.
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
			// if this agent leads again later, it starts its looks afresh
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

// pass looks at each move once and returns how soon the next deadline
// comes.
func (m *mover) pass(ctx context.Context) time.Duration {
	copied := m.cluster.SharedCopy()
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

func (m *mover) step(ctx context.Context, copied *state.State, operation *state.Operation, now time.Time) error {
	move := operation.Move
	stack, ok := copied.Stacks[move.Stack]
	down, isDown := downHost(copied, move)
	switch {
	case operation.State == state.OpRollingBack:
		// nothing to undo: the stack runs on its old host again (meanwhile
		// its status shows running, as after a restart). Once the locks are
		// free, the placement gives up the copies that the move left.
		return m.finish(ctx, operation, true, "")
	case !ok:
		return m.finish(ctx, operation, true, "the stack is gone")
	case isDown && operation.Step < state.MoveHost:
		return m.abandon(ctx, operation, fmt.Sprintf("host %s is %s", down.Name, down.Phase))
	case isDown && down.ID == move.To:
		// the stack belongs to the new host now: the failover takes it from there
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
	// compute in seconds first: bytes times the nanoseconds of a second
	// would overflow from a few GiB on
	return state.StepTime + time.Duration(2*total/rate+1)*time.Second
}

// setHost runs the step after the old host has stopped the stack.
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

// late ends a move past its point of no return as failed: the stack stays
// where the move left it, and the placement takes it from there.
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

// lateResync returns why a late move should roll back; an empty reason gives
// it one more progressWindow.
//
// If the state already knows the copies as synced, only the images are
// still missing, and the move gets one window. Otherwise the answers of the
// new host are compared:
//   - The first look gets a window, even without an answer (a network
//     hiccup). So does the first answer after a look without one.
//   - Less left than last time gets a window. The same or more rolls back,
//     and so does a second look without an answer.
//
// Each change of leader gives at most one more window.
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
	// answered is true when the new host told how many bytes are left
	answered bool
	left     int64
	// synced: the state knew the copies as synced, so only the images were
	// missing
	synced bool
}

// copiesSynced skips a volume that never held data: it has nothing to sync.
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

// rollBack rolls a move back: the stack keeps its old host or gets it back.
func (m *mover) rollBack(ctx context.Context, operation *state.Operation, reason string) error {
	m.logger.Warn("a move rolls back", "operation", operation.ID, "stack", operation.Move.Stack, "reason", reason)
	_, err := commit(ctx, m.propose, state.KindRollBackOperation, state.RollBackOperation{
		ID: operation.ID, FromStep: operation.Step, Error: reason,
	})
	return err
}

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

// downHost returns a host of the move that is no longer in service. The move
// cannot wait for it to come back.
func downHost(copied *state.State, move *state.StackMove) (*state.Host, bool) {
	ids := []state.HostID{move.To}
	if move.From != nil {
		ids = append(ids, *move.From)
	}
	for _, id := range ids {
		if host := copied.Hosts[id]; !state.InService(host.Phase) {
			return host, true
		}
	}
	return nil, false
}

// abandon keeps the stack on its old host. If the old host is the one that
// is down, the failover takes the stack once the host is fenced.
func (m *mover) abandon(ctx context.Context, operation *state.Operation, reason string) error {
	if operation.Step >= state.MoveStopped {
		return m.finish(ctx, operation, true, reason)
	}
	return m.rollBack(ctx, operation, reason)
}
