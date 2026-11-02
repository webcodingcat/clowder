package agent

import (
	"context"
	"sync"
	"time"

	"github.com/webcodingcat/clowder/internal/controller"
	"github.com/webcodingcat/clowder/internal/state"
)

// What the agent's loops share.

// runPasses runs pass at once. After that, it runs pass again whenever the
// state changes, wake fires, or reconcileInterval passes, until ctx ends.
// wake may be nil.
//
// The host's report has a loop of its own: it reacts only to a new
// cluster.yaml, not to every change of the state (hostReporter.run).
func runPasses(ctx context.Context, cluster stateReader, wake <-chan struct{}, pass func()) {
	for {
		// take the channel before reading, so no change between goes unseen
		changed := cluster.Changed()
		pass()
		select {
		case <-ctx.Done():
			return
		case <-changed:
		case <-wake:
		case <-time.After(reconcileInterval):
		}
	}
}

// passSignal is how the supervisor reaches a runner (the stack runner,
// Traefik's): it starts the runner's next pass at once and counts Docker's
// restarts. A restart stops every container, and nothing starts them again
// (restart: "no"), so the runner brings up anew what it brought up before.
type passSignal struct {
	// wake is buffered (1): one pass due is enough
	wake chan struct{}

	mu             sync.Mutex
	dockerRestarts int
}

func newPassSignal() *passSignal {
	return &passSignal{wake: make(chan struct{}, 1)}
}

// now starts a pass at once, unless one is due already.
func (s *passSignal) now() {
	select {
	case s.wake <- struct{}{}:
	default: // a pass is due already
	}
}

// dockerRestarted counts a restart of Docker and starts a pass.
func (s *passSignal) dockerRestarted() {
	s.mu.Lock()
	s.dockerRestarts++
	s.mu.Unlock()
	s.now()
}

// restarts is how often Docker restarted since the agent started.
func (s *passSignal) restarts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dockerRestarts
}

// broughtUp is what a runner last brought up: the hash of everything
// compose up read, and how often Docker had restarted before compose up
// began. If Docker restarts again, the containers are gone, so it no
// longer matches.
type broughtUp struct {
	hash           [32]byte
	dockerRestarts int
}

// pauser is what the runners need from the supervisor;
// supervisor.Supervisor is the real one.
type pauser interface {
	Pause(project string) (resume func())
}

// whilePaused runs change, which stops or recreates a project's containers,
// with the supervisor paused for the project: those containers did not
// crash.
func whilePaused(watcher pauser, project string, change func() error) error {
	resume := watcher.Pause(project)
	defer resume()
	return change()
}

// commitAsHost commits a command in which this host reports about itself
// (its report, a stack's status, a forgotten copy), with the host as actor.
// The leader's loops commit with the leader as actor (controller's commit).
func commitAsHost(ctx context.Context, propose controller.Propose, host state.HostID, kind string, payload any) error {
	cmd, err := state.NewCommand(kind, time.Time{}, state.HostActor(host), payload)
	if err != nil {
		return err
	}
	_, err = propose(ctx, cmd)
	return err
}
