package controller

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/webcodingcat/clowder/internal/scheduler"
	"github.com/webcodingcat/clowder/internal/state"
)

// comeBackAfter is how long a pushed-out service must fit before it comes
// back. The free room changes only with the state, so this bridges only a
// short dip, such as a stack being replaced.
const comeBackAfter = 60 * time.Second

// placer is the leader's placement and what it keeps in memory between
// passes; a new leader starts afresh.
type placer struct {
	cluster Cluster
	propose Propose
	logger  *slog.Logger
	// fitting is the pushed-out service that fits ("stack/service"), and
	// since when
	fitting      string
	fittingSince time.Time
	// conditions hold the hosts whose highest priority alone reserves more
	// than they offer, for the alert
	conditions *Conditions
}

// RunPlacement places stacks and their new volumes, pushes services out
// and brings them back while this agent leads, until ctx ends: at every
// change of the state and every interval.
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

// pass plans once and commits the decisions and why stacks wait. It tells
// how soon it should look again: when a pushed-out service will have fit
// long enough, else 0.
func (p *placer) pass(ctx context.Context) time.Duration {
	// Plan weighs every stack on every host: too slow for the state's lock,
	// which Raft's Apply waits for
	copied, err := p.cluster.SharedCopy()
	if err != nil {
		p.logger.Error("place the stacks", "error", err)
		return 0
	}
	decisions := scheduler.Plan(copied)
	p.noteOverCapacity(copied, decisions.OverCapacity)
	// A push-out holds, for each stack it touches, the stack's new list of
	// pushed-out services. If one fails, nothing is placed in this pass:
	// the placements count on the room it frees. A placement that fails, by
	// contrast, stands alone, so the others still go.
	for _, pushOut := range decisions.PushOuts {
		p.logger.Warn("pushing out services: the host's stacks reserve more than it offers", "pushed_out", pushOut)
		if !p.commitOrLog(ctx, state.KindSetPushedOut, state.SetPushedOut{Stacks: pushOut}) {
			return 0
		}
	}
	p.commitWaitReasons(ctx, copied, decisions.Waiting)
	for _, placement := range decisions.Placements {
		p.commitPlacement(ctx, placement)
	}
	return p.comeBack(ctx, decisions.ComeBack)
}

// commitOrLog writes one command and tells whether it committed; a failure
// is logged, and the next pass plans again.
func (p *placer) commitOrLog(ctx context.Context, kind string, payload any) bool {
	if err := commit(ctx, p.propose, kind, payload); err != nil {
		p.logger.Error("place the stacks", "command", kind, "error", err)
		return false
	}
	return true
}

// noteOverCapacity holds the condition of each host over capacity and logs
// it once.
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

// commitWaitReasons records why stacks wait. The state remembers the last
// reason, so each change is written and logged once, not every 30 s, and a
// new leader knows it too; an empty reason clears it once the stack no
// longer waits. Every change is a Raft write and an audit entry, so a
// reason holds only what stays the same from pass to pass (no free space,
// no times).
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

// commitPlacement commits one placement: what the stack pushes out first,
// then the volumes, then the stack's host, so the agent finds the volumes
// when it sees the stack. If the leader changes in between, the services
// stay pushed out without the new stack, and come back (comeBack). The
// first command that fails ends the placement; the next pass plans it
// again.
func (p *placer) commitPlacement(ctx context.Context, placement scheduler.Placement) {
	if placement.PushedOut != nil &&
		!p.commitOrLog(ctx, state.KindSetPushedOut, state.SetPushedOut{Stacks: placement.PushedOut}) {
		return
	}
	for _, volume := range placement.Volumes {
		if !p.commitOrLog(ctx, state.KindPutVolume, state.PutVolume{Volume: volume}) {
			return
		}
	}
	if placement.SetsHost {
		host := placement.Host
		p.commitOrLog(ctx, state.KindSetStackStatus, state.SetStackStatus{
			ID: placement.Stack, Status: state.StackStarting, Host: &host,
		})
	}
}

// comeBack brings a pushed-out service back once it has fit for
// comeBackAfter, one service per decision: its return changes the free
// room, so the next one's wait starts again. It tells how long until the
// service will have fit long enough, or 0.
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
	p.commitOrLog(ctx, state.KindSetPushedOut, state.SetPushedOut{Stacks: map[string][]string{back.Stack: back.PushedOut}})
	p.fitting = ""
	return 0
}
