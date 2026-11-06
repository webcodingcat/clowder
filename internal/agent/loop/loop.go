// Package loop is what the agent's loops share: how they read the state,
// when they run a pass, how the supervisor wakes them, and how a host
// commits about itself.
package loop

import (
	"context"
	"sync"
	"time"

	"github.com/webcodingcat/clowder/internal/state"
)

// Interval is how often the reconcilers (network, stacks, Traefik) run
// without a change of the state, to repair what someone changed by hand.
const Interval = 30 * time.Second

// Cluster is what the agent's loops need from the Raft member. A loop
// reads the state through SharedCopy and never changes it.
type Cluster interface {
	Read(use func(current *state.State))
	SharedCopy() (*state.State, error)
	Changed() <-chan struct{}
}

// Run runs pass at once. After that, it runs pass again whenever the state
// changes, wake fires, or Interval passes, until ctx ends. wake may be nil.
//
// The host's report has its own loop, because it reacts only to a new
// configuration, not to every change of the state.
func Run(ctx context.Context, cluster Cluster, wake <-chan struct{}, pass func()) {
	for {
		// take the channel before reading, so no change in between is missed
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

// Signal is how the supervisor reaches a runner (the stack runner or
// Traefik's runner). It starts the runner's next pass at once and counts
// Docker's restarts. With live-restore the containers outlive a restart,
// but nobody saw the containers that died in the meantime, and nothing
// starts them again (restart: "no"). So after a restart the runner brings
// up again everything it brought up before.
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

// Wake fires when a pass is due; it is Run's wake.
func (s *Signal) Wake() <-chan struct{} { return s.wake }

// Now starts a pass at once, unless one is due already.
func (s *Signal) Now() {
	select {
	case s.wake <- struct{}{}:
	default: // a pass is due already
	}
}

// DockerRestarted counts a restart of Docker and starts a pass.
func (s *Signal) DockerRestarted() {
	s.mu.Lock()
	s.dockerRestarts++
	s.mu.Unlock()
	s.Now()
}

// Restarts is how often Docker restarted since the agent started.
func (s *Signal) Restarts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dockerRestarts
}

// BroughtUp is what a runner last brought up: the hash of everything
// compose up read, and how often Docker had restarted before compose up
// began. If Docker restarts again, the containers are gone, so the value no
// longer matches.
type BroughtUp struct {
	Hash           [32]byte
	DockerRestarts int
}

// Pauser is what the runners need from the supervisor.
// supervisor.Supervisor is the real implementation.
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

// CommitAsHost commits a command in which this host reports about itself
// (its report, a stack's status, a forgotten copy), with the host as actor.
// The leader's loops commit with the leader as actor (controller's commit).
// propose is a controller.Propose. Its type is spelled out, so the host's
// loops need nothing from the leader's package.
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
