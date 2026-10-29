// Package controller holds the leader's loops: they read the state, decide
// (with internal/scheduler) and write commands. They never change the host;
// the agents do what the commands say.
package controller

import (
	"context"
	"time"

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

// commit writes one command as the leader.
func commit(ctx context.Context, propose Propose, kind string, payload any) error {
	// the time comes from the leader as it commits
	cmd, err := state.NewCommand(kind, time.Time{}, state.LeaderActor, payload)
	if err != nil {
		return err
	}
	_, err = propose(ctx, cmd)
	return err
}
