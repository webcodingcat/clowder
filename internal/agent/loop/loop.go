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
// reads the state through SharedCopy and changes nothing of it.
type Cluster interface {
	Read(use func(current *state.State))
	SharedCopy() (*state.State, error)
	Changed() <-chan struct{}
}

// Run runs pass at once. After that, it runs pass again whenever the state
// changes, wake fires, or Interval passes, until ctx ends. wake may be nil.
//
// The host's report has a loop of its own: it reacts only to a new
// cluster.yaml, not to every change of the state.
func Run(ctx context.Context, cluster Cluster, wake <-chan struct{}, pass func()) {
	for {
		// take the channel before reading, so no change between goes unseen
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

// Signal is how the supervisor reaches a runner (the stack runner,
// Traefik's): it starts the runner's next pass at once and counts Docker's
// restarts. With live-restore the containers outlive a restart, but what
// died meanwhile went unseen and nothing starts it again (restart: "no"),
// so the runner brings up anew what it brought up before.
type Signal struct {
	// wake is buffered (1): one pass due is enough
	wake chan struct{}

	mu             sync.Mutex
	dockerRestarts int
}

// NewSignal has no pass due and counts no restart.
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
// began. If Docker restarts again, the containers are gone, so it no
// longer matches.
type BroughtUp struct {
	Hash           [32]byte
	DockerRestarts int
}

// Pauser is what the runners need from the supervisor;
// supervisor.Supervisor is the real one.
type Pauser interface {
	Pause(project string) (resume func())
}

// WhilePaused runs change, which stops or recreates a project's containers,
// with the supervisor paused for the project: those containers did not
// crash.
func WhilePaused(watcher Pauser, project string, change func() error) error {
	resume := watcher.Pause(project)
	defer resume()
	return change()
}

// CommitAsHost commits a command in which this host reports about itself
// (its report, a stack's status, a forgotten copy), with the host as actor.
// The leader's loops commit with the leader as actor (controller's commit).
// propose is a controller.Propose; spelled out, so the host's loops need
// nothing of the leader's package.
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
