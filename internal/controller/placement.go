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
			if again := p.place(ctx); again > 0 {
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

// place plans once and writes the decisions and why stacks wait. It tells
// how soon it should look again: when a pushed-out service will have fit
// long enough, else 0.
func (p *placer) place(ctx context.Context) time.Duration {
	// Plan weighs every stack on every host: too slow for the state's lock,
	// which Raft's Apply waits for
	var copied *state.State
	var err error
	p.cluster.Read(func(current *state.State) { copied, err = current.Copy() })
	if err != nil {
		p.logger.Error("place the stacks", "error", err)
		return 0
	}
	decisions := scheduler.Plan(copied)
	p.noteOverCapacity(copied, decisions.OverCapacity)
	// each push-out is the new lists of the stacks it touches, by stack
	for _, pushOut := range decisions.PushOuts {
		p.logger.Warn("pushing out services: the host's stacks reserve more than it offers", "pushed_out", pushOut)
		if err := commit(ctx, p.propose, state.KindSetPushedOut, state.SetPushedOut{Stacks: pushOut}); err != nil {
			// the placements below count on the room it frees; the next
			// pass plans again
			p.logger.Error("place the stacks", "command", state.KindSetPushedOut, "error", err)
			return 0
		}
	}
	p.writeWaitReasons(ctx, copied, decisions.Waiting)
	for _, placement := range decisions.Placements {
		if err := p.write(ctx, placement); err != nil {
			p.logger.Error("place a stack", "stack", placement.Stack, "error", err)
		}
	}
	return p.comeBack(ctx, decisions.ComeBack)
}

// commitOrLog writes one command; a failure is logged, and the next pass
// plans again.
func (p *placer) commitOrLog(ctx context.Context, kind string, payload any) {
	if err := commit(ctx, p.propose, kind, payload); err != nil {
		p.logger.Error("place the stacks", "command", kind, "error", err)
	}
}

// noteOverCapacity holds the condition of each host over capacity and logs
// it once.
func (p *placer) noteOverCapacity(copied *state.State, hosts []state.HostID) {
	for _, id := range slices.Sorted(maps.Keys(copied.Hosts)) {
		if p.conditions.Hold(overCapacityKey(id), slices.Contains(hosts, id)) {
			p.logger.Warn("host over capacity: its highest priority alone reserves more than it offers",
				"host", copied.Hosts[id].Name)
		}
	}
}

// writeWaitReasons records why stacks wait. The state remembers the last
// reason, so each change is written and logged once, not every 30 s, and a
// new leader knows it too; an empty reason clears it once the stack no
// longer waits. Every change is a Raft write and an audit entry, so a
// reason holds only what stays the same from pass to pass (no free space,
// no times).
func (p *placer) writeWaitReasons(ctx context.Context, copied *state.State, waiting []scheduler.Waiting) {
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

// write commits one placement: what the stack pushes out first, then the
// volumes, then the stack's host, so the agent finds the volumes when it
// sees the stack. If the leader changes in between, the services stay
// pushed out without the new stack, and come back (comeBack).
func (p *placer) write(ctx context.Context, placement scheduler.Placement) error {
	if placement.PushedOut != nil {
		err := commit(ctx, p.propose, state.KindSetPushedOut, state.SetPushedOut{Stacks: placement.PushedOut})
		if err != nil {
			return err
		}
	}
	for _, volume := range placement.Volumes {
		if err := commit(ctx, p.propose, state.KindPutVolume, state.PutVolume{Volume: volume}); err != nil {
			return err
		}
	}
	if !placement.Place {
		return nil
	}
	host := placement.Host
	return commit(ctx, p.propose, state.KindSetStackStatus, state.SetStackStatus{
		ID: placement.Stack, Status: state.StackStarting, Host: &host,
	})
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
