// Package controller holds the leader's loops. They read the state, decide
// (with internal/scheduler) and write commands. They never change the host;
// the agents carry out what the commands say.
package controller

import (
	"context"
	"time"

	"github.com/webcodingcat/clowder/internal/passes"
	"github.com/webcodingcat/clowder/internal/state"
)

// Cluster is what the loops need from the agent's Raft member. A loop
// reads the state through SharedCopy and never changes that copy.
type Cluster interface {
	Read(use func(current *state.State))
	SharedCopy() *state.State
	Changed() <-chan struct{}
	IsLeader() bool
	// VerifyLeader makes sure that a majority still accepts this agent as
	// leader. A loop calls it before it powers off a server or moves the
	// Floating IP.
	VerifyLeader(ctx context.Context) error
}

// Propose commits a command (api.Peers.Propose).
type Propose func(ctx context.Context, cmd state.Command) (uint64, error)

// Open decrypts a secret of the state (datakey.Key.Decrypt).
type Open func(encrypted state.Encrypted) ([]byte, error)

// runWhileLeading runs the leader's part of a loop with the passes of
// internal/passes: lead while this agent leads, which returns the wait until
// the next pass, and follow while it does not, which drops what the loop
// keeps in memory (follow may be nil). While this agent follows, it runs a
// pass after each wait of length every, or sooner when the state changes.
func runWhileLeading(ctx context.Context, cluster Cluster, every time.Duration,
	lead func(ctx context.Context) time.Duration, follow func(),
) {
	passes.Run(ctx, cluster.Changed, nil, func() time.Duration {
		if cluster.IsLeader() {
			return lead(ctx)
		}
		if follow != nil {
			follow()
		}
		return every
	})
}

func commit(ctx context.Context, propose Propose, kind string, payload any) (uint64, error) {
	// the leader sets the time when it commits
	cmd, err := state.NewCommand(kind, time.Time{}, state.LeaderActor, payload)
	if err != nil {
		return 0, err
	}
	return propose(ctx, cmd)
}
