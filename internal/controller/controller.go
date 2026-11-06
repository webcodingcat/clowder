// Package controller holds the leader's loops: they read the state, decide
// (with internal/scheduler) and write commands. They never change the host;
// the agents do what the commands say.
package controller

import (
	"context"
	"time"

	"github.com/webcodingcat/clowder/internal/state"
)

// Cluster is what the loops need from the agent's Raft member. A loop
// reads the state through SharedCopy and never changes that copy.
type Cluster interface {
	Read(use func(current *state.State))
	SharedCopy() (*state.State, error)
	Changed() <-chan struct{}
	IsLeader() bool
	// VerifyLeader makes sure that a majority still accepts this agent as
	// leader. A loop calls it before it powers off a server or moves the
	// Floating IP.
	VerifyLeader(ctx context.Context) error
}

// Propose commits a command (api.Server.Propose).
type Propose func(ctx context.Context, cmd state.Command) (uint64, error)

// Open decrypts a secret of the state (datakey.Key.Decrypt).
type Open func(encrypted state.Encrypted) ([]byte, error)

// interval is how often the loops look while the state does not change.
const interval = 30 * time.Second

// commit writes one command as the leader and returns its Raft index.
func commit(ctx context.Context, propose Propose, kind string, payload any) (uint64, error) {
	// the time comes from the leader as it commits
	cmd, err := state.NewCommand(kind, time.Time{}, state.LeaderActor, payload)
	if err != nil {
		return 0, err
	}
	return propose(ctx, cmd)
}
