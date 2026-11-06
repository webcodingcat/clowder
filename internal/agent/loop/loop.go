// Package loop holds what the agent's loops share: how they read the state,
// when they run a pass, how the supervisor wakes them, and how a host
// commits commands about itself.
package loop

import (
	"context"
	"sync"
	"time"

	"github.com/webcodingcat/clowder/internal/state"
)

// Interval is how often the agent's loops run a pass when the state does
// not change. These passes repair what someone changed by hand.
const Interval = 30 * time.Second

// Cluster is what the agent's loops need from the Raft member. A loop
// reads the state through SharedCopy and never changes it.
type Cluster interface {
	Read(use func(current *state.State))
	SharedCopy() *state.State
	Changed() <-chan struct{}
}

// Run runs pass immediately. After that, it runs pass again whenever the
// state changes, wake fires, or Interval passes, until ctx ends. wake may be
// nil.
func Run(ctx context.Context, cluster Cluster, wake <-chan struct{}, pass func()) {
	for {
		// get the channel before the pass, so no change during it is missed
		changed := cluster.Changed()
		pass()
		select {
		case <-ctx.Done():
			return
		case <-changed:
		case <-wake:
		case <-time.After(Interval):
		}
	}
}

// Signal is how the supervisor reaches a runner. It counts Docker's
// restarts, because nobody watched the containers that died during a
// restart and nothing starts them again (restart: "no"), so the runner
// brings up again everything it brought up before.
type Signal struct {
	// wake is buffered (1): one pending pass is enough
	wake chan struct{}

	mu             sync.Mutex
	dockerRestarts int
}

// NewSignal returns a signal with no pending pass and no counted restart.
func NewSignal() *Signal {
	return &Signal{wake: make(chan struct{}, 1)}
}

// Wake fires when a pass is due. It is the wake channel for Run.
func (s *Signal) Wake() <-chan struct{} { return s.wake }

// Now starts a pass immediately, unless one is already due.
func (s *Signal) Now() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// DockerRestarted counts a restart of Docker and starts a pass.
func (s *Signal) DockerRestarted() {
	s.mu.Lock()
	s.dockerRestarts++
	s.mu.Unlock()
	s.Now()
}

// Restarts returns how often Docker has restarted since the agent started.
func (s *Signal) Restarts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dockerRestarts
}

// BroughtUp records what a runner last brought up: the hash of everything
// that compose up read, and how often Docker had restarted before compose
// up began. If Docker restarts again, the containers are gone, so the value
// no longer matches.
type BroughtUp struct {
	Hash           [32]byte
	DockerRestarts int
}

// Pauser is what the runners need from the supervisor.
type Pauser interface {
	Pause(project string) (resume func())
}

// WhilePaused runs change, which stops or recreates a project's containers,
// while the supervisor is paused for the project. Those containers did not
// crash, so the supervisor must not restart them.
func WhilePaused(watcher Pauser, project string, change func() error) error {
	resume := watcher.Pause(project)
	defer resume()
	return change()
}

// CommitAsHost commits a command in which this host reports about itself,
// with the host as actor. propose is a controller.Propose, spelled out so
// that the host's loops need nothing from the leader's package.
func CommitAsHost(ctx context.Context, propose func(context.Context, state.Command) (uint64, error),
	host state.HostID, kind string, payload any,
) error {
	cmd, err := state.NewCommand(kind, time.Time{}, state.HostActor(host), payload)
	if err != nil {
		return err
	}
	_, err = propose(ctx, cmd)
	return err
}
