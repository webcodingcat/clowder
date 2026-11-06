package controller

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/scheduler"
	"github.com/webcodingcat/clowder/internal/state"
)

// comeBackAfter is how long a pushed-out service must fit before it comes
// back. The free room changes only when the state changes, so this wait
// only bridges a short dip, for example while a stack is replaced.
const comeBackAfter = 60 * time.Second

// placer is the leader's placement and what it keeps in memory between
// passes. A new leader starts with an empty memory.
type placer struct {
	cluster Cluster
	propose Propose
	logger  *slog.Logger
	// fitting is the pushed-out service that fits ("stack/service"), and
	// fittingSince is when it began to fit
	fitting      string
	fittingSince time.Time
	conditions   *Conditions
	// plannedAt is the index of the state that the pass plans with, so the
	// state can refuse its commands as stale (state.ErrStale)
	plannedAt uint64
}

// RunPlacement places stacks and their new volumes, pushes services out
// and brings them back while this agent leads, until ctx ends.
func RunPlacement(ctx context.Context, cluster Cluster, propose Propose, conditions *Conditions, logger *slog.Logger) {
	p := &placer{cluster: cluster, propose: propose, logger: logger, conditions: conditions}
	for {
		changed := cluster.Changed()
		wait := interval
		if cluster.IsLeader() {
			if again := p.pass(ctx); again > 0 {
				wait = min(wait, again)
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

// pass returns how soon the next pass should run: when a pushed-out service
// will have fit long enough, or else 0.
func (p *placer) pass(ctx context.Context) time.Duration {
	// Plan weighs every stack on every host. That is too slow to do under
	// the state's lock, which Raft's Apply waits for.
	copied := p.cluster.SharedCopy()
	p.plannedAt = copied.Index
	decisions := scheduler.Plan(copied, time.Now())
	p.noteOverCapacity(copied, decisions.OverCapacity)
	// If a push-out fails, nothing is placed in this pass, because the
	// placements count on the room it frees. A failed placement affects only
	// its own stack.
	for _, pushOut := range decisions.PushOuts {
		p.logger.Warn("pushing out services: the host's stacks reserve more than it offers", "pushed_out", pushOut)
		if !p.commitOrLog(ctx, state.KindSetPushedOut, state.SetPushedOut{Stacks: pushOut, PlannedAt: p.plannedAt}) {
			return 0
		}
	}
	p.commitWaitReasons(ctx, copied, decisions.Waiting)
	for _, placement := range decisions.Placements {
		p.commitPlacement(ctx, placement)
	}
	for _, move := range decisions.Moves {
		p.startMove(ctx, copied, move)
	}
	for _, record := range decisions.Copies {
		p.logger.Info("the copies of a volume change", "volume", record.ID(), "copies", record.Copies)
		p.commitOrLog(ctx, state.KindPutVolume, state.PutVolume{Volume: record, PlannedAt: p.plannedAt})
	}
	return p.comeBack(ctx, decisions.ComeBack)
}

// commitOrLog reports whether the command committed; after a failure, the
// next pass plans again. A command at the index right after plannedAt
// becomes part of the planned state, so the pass's next commands may change
// what it changed.
func (p *placer) commitOrLog(ctx context.Context, kind string, payload any) bool {
	index, err := commit(ctx, p.propose, kind, payload)
	switch {
	case errors.Is(err, state.ErrStale):
		p.logger.Info("the state changed since the plan; planning again", "command", kind, "reason", err)
		return false
	case err != nil:
		p.logger.Error("place the stacks", "command", kind, "error", err)
		return false
	}
	if index == p.plannedAt+1 {
		p.plannedAt = index
	}
	return true
}

func (p *placer) noteOverCapacity(copied *state.State, hosts []state.HostID) {
	for _, id := range slices.Sorted(maps.Keys(copied.Hosts)) {
		if copied.Hosts[id].Phase == state.Removed {
			continue // its condition is forgotten (conditionKeys)
		}
		if p.conditions.Observe(overCapacityKey(id), slices.Contains(hosts, id)) {
			p.logger.Warn("host over capacity: its highest priority alone reserves more than it offers",
				"host", copied.Hosts[id].Name)
		}
	}
}

// commitWaitReasons writes a reason only when it changes, because each
// change is a Raft write and an audit entry. So a reason holds only what
// stays the same from pass to pass: no free space, no times.
func (p *placer) commitWaitReasons(ctx context.Context, copied *state.State, waiting []scheduler.Waiting) {
	reasons := map[string]string{}
	for _, wait := range waiting {
		reasons[wait.Stack] = wait.Reason
	}
	for _, id := range slices.Sorted(maps.Keys(copied.Stacks)) {
		reason := reasons[id]
		if copied.Stacks[id].WaitReason == reason {
			continue
		}
		if reason != "" {
			p.logger.Warn("stack waits for a place", "stack", id, "reason", reason)
		}
		p.commitOrLog(ctx, state.KindSetStackWaitReason, state.SetStackWaitReason{ID: id, Reason: reason})
	}
}

// commitPlacement commits the stack's host last, so that the agent finds
// the volumes when it sees the stack. If the leader changes in between, the
// services stay pushed out without the new stack, and come back later
// (comeBack).
func (p *placer) commitPlacement(ctx context.Context, placement scheduler.Placement) {
	if placement.PushedOut != nil &&
		!p.commitOrLog(ctx, state.KindSetPushedOut, state.SetPushedOut{Stacks: placement.PushedOut, PlannedAt: p.plannedAt}) {
		return
	}
	for _, volume := range placement.Volumes {
		if !p.commitOrLog(ctx, state.KindPutVolume, state.PutVolume{Volume: volume, PlannedAt: p.plannedAt}) {
			return
		}
	}
	if placement.SetsHost {
		host := placement.Host
		p.commitOrLog(ctx, state.KindSetStackStatus, state.SetStackStatus{
			ID: placement.Stack, Status: state.StackStarting, Host: &host, PlannedAt: p.plannedAt,
		})
	}
}

// comeBack brings back one service per decision, because each return
// changes the free room, so the next service's wait starts again. It
// returns how long until the service will have fit long enough, or 0.
func (p *placer) comeBack(ctx context.Context, back *scheduler.ComeBack) time.Duration {
	if back == nil {
		p.fitting = ""
		return 0
	}
	name := back.Stack + "/" + back.Service
	now := time.Now()
	if p.fitting != name {
		p.fitting, p.fittingSince = name, now
	}
	if left := comeBackAfter - now.Sub(p.fittingSince); left > 0 {
		return left
	}
	p.logger.Info("bringing a pushed-out service back", "stack", back.Stack, "service", back.Service)
	p.commitOrLog(ctx, state.KindSetPushedOut, state.SetPushedOut{
		Stacks: map[string][]string{back.Stack: back.PushedOut}, PlannedAt: p.plannedAt,
	})
	p.fitting = ""
	return 0
}

// startMove starts a move-stack operation, which RunMoves carries out. If
// the start fails after the push-out, the pushed-out services come back by
// the usual rule (comeBack).
func (p *placer) startMove(ctx context.Context, copied *state.State, move scheduler.Move) {
	if move.PushedOut != nil &&
		!p.commitOrLog(ctx, state.KindSetPushedOut, state.SetPushedOut{Stacks: move.PushedOut, PlannedAt: p.plannedAt}) {
		return
	}
	stack := copied.Stacks[move.Stack]
	locks := []string{state.StackLock(move.Stack)}
	for _, name := range stack.Spec.VolumeNames() {
		locks = append(locks, state.VolumeLock(names.ID(stack.Namespace, name)))
	}
	p.logger.Info("moving a stack", "stack", move.Stack, "to", copied.Hosts[move.To].Name, "why", move.Why)
	p.commitOrLog(ctx, state.KindStartOperation, state.StartOperation{
		Kind: state.OperationMoveStack, Locks: locks,
		Move: &state.StackMove{Stack: move.Stack, From: move.From, To: move.To},
	})
}
