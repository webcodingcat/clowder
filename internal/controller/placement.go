// Package controller holds the leader's loops: they read the state, decide
// (with internal/scheduler) and write commands. They never change the host;
// the agents do what the commands say.
package controller

import (
	"context"
	"log/slog"
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
	reported := map[string]string{} // the reason each stack last waited for
	for {
		changed := cluster.Changed()
		if cluster.IsLeader() {
			place(ctx, cluster, propose, logger, reported)
		}
		select {
		case <-ctx.Done():
			return
		case <-changed:
		case <-time.After(interval):
		}
	}
}

// place plans once and writes the decisions.
func place(ctx context.Context, cluster Cluster, propose Propose, logger *slog.Logger, reported map[string]string) {
	var placements []scheduler.Placement
	var waiting []scheduler.Waiting
	cluster.Read(func(current *state.State) {
		placements, waiting = scheduler.Plan(ctx, current)
	})
	for _, wait := range waiting {
		// say it once per reason, not every 30 s
		if reported[wait.Stack] != wait.Reason {
			logger.Warn("stack waits for a place", "stack", wait.Stack, "reason", wait.Reason)
			reported[wait.Stack] = wait.Reason
		}
	}
	for _, placement := range placements {
		delete(reported, placement.Stack)
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
