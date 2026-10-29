// Package controller holds the leader's loops: they read the state, decide
// (with internal/scheduler) and write commands. They never change the host;
// the agents do what the commands say.
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

// Cluster is what the loops need from the agent's Raft member.
type Cluster interface {
	Read(use func(current *state.State))
	Changed() <-chan struct{}
	IsLeader() bool
}

// Propose commits a command (api.Server.Propose).
type Propose func(ctx context.Context, cmd state.Command) (uint64, error)

// interval is how often the loops look without a change of the state.
const interval = 30 * time.Second

// RunPlacement places stacks and their new volumes while this agent leads,
// until ctx ends: at every change of the state and every interval.
func RunPlacement(ctx context.Context, cluster Cluster, propose Propose, logger *slog.Logger) {
	for {
		changed := cluster.Changed()
		if cluster.IsLeader() {
			place(ctx, cluster, propose, logger)
		}
		select {
		case <-ctx.Done():
			return
		case <-changed:
		case <-time.After(interval):
		}
	}
}

// place plans once and writes the decisions and why stacks wait.
func place(ctx context.Context, cluster Cluster, propose Propose, logger *slog.Logger) {
	// Plan parses every stack: too slow for the state's lock, which Raft's
	// Apply waits for
	var copied *state.State
	var err error
	cluster.Read(func(current *state.State) { copied, err = current.Copy() })
	if err != nil {
		logger.Error("place the stacks", "error", err)
		return
	}
	placements, waiting := scheduler.Plan(ctx, copied)
	reasons := map[string]string{}
	for _, wait := range waiting {
		reasons[wait.Stack] = wait.Reason
	}
	// the state remembers the last reason, so each change is written and
	// logged once, not every 30 s, and a new leader knows it too; an empty
	// reason clears it once the stack no longer waits. Every change is a Raft
	// write and an audit entry, so a reason holds only what stays the same
	// from pass to pass (no free space, no times).
	for _, name := range slices.Sorted(maps.Keys(copied.Stacks)) {
		reason := reasons[name]
		if copied.Stacks[name].WaitReason == reason {
			continue
		}
		if reason != "" {
			logger.Warn("stack waits for a place", "stack", name, "reason", reason)
		}
		err := commit(ctx, propose, state.KindSetStackWaitReason, state.SetStackWaitReason{Name: name, Reason: reason})
		if err != nil {
			logger.Error("record why a stack waits", "stack", name, "error", err)
		}
	}
	for _, placement := range placements {
		if err := write(ctx, propose, placement); err != nil {
			logger.Error("place a stack", "stack", placement.Stack, "error", err)
		}
	}
}

// write commits one placement: the volumes first, then the stack's host,
// so the agent finds the volumes when it sees the stack.
func write(ctx context.Context, propose Propose, placement scheduler.Placement) error {
	for _, volume := range placement.Volumes {
		if err := commit(ctx, propose, state.KindPutVolume, state.PutVolume{Volume: volume}); err != nil {
			return err
		}
	}
	if !placement.Place {
		return nil
	}
	host := placement.Host
	return commit(ctx, propose, state.KindSetStackStatus, state.SetStackStatus{
		Name: placement.Stack, Status: state.StackStarting, Host: &host,
	})
}

func commit(ctx context.Context, propose Propose, kind string, payload any) error {
	// the time comes from the leader as it commits
	cmd, err := state.NewCommand(kind, time.Time{}, state.LeaderActor, payload)
	if err != nil {
		return err
	}
	_, err = propose(ctx, cmd)
	return err
}
