package controller

import (
	"sync"
	"time"

	"github.com/webcodingcat/clowder/internal/state"
)

// How hosts tell they are alive, and when the leader takes one for failed.
// The beats go to every voter, not through the Raft log: a new leader has
// heard them too, so the time a host has been silent runs on across a
// change of leader.
const (
	// BeatInterval is how often an agent tells every voter it is alive.
	BeatInterval = 5 * time.Second
	// FailAfter is how long a majority of the voters must not have heard a
	// host before it is failed: well above a pause of the network or of
	// all the hosts (20 s), and below the time systemd takes to restart a
	// hung agent (WatchdogSec) plus its start.
	FailAfter = 45 * time.Second
	// backWithin is how recently a majority of the voters must have heard a
	// failed host for it to be active again: a few beats, so one beat that
	// slips through a flaky network does not bring it back.
	backWithin = 3 * BeatInterval
)

// Heard is when this agent last heard each host's beat. It is in memory:
// after a start it counts from the start, as a host it has not heard yet
// may well be alive, and the others' voters hold the earlier beats.
type Heard struct {
	mu      sync.Mutex
	started time.Time
	last    map[state.HostID]time.Time
	// now is time.Now; tests replace it
	now func() time.Time
}

// NewHeard has heard no host yet.
func NewHeard() *Heard {
	return &Heard{started: time.Now(), last: map[state.HostID]time.Time{}, now: time.Now}
}

// Beat records that a host is alive (the agent's health endpoint).
func (h *Heard) Beat(host state.HostID) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.last[host] = h.now()
}

// Silence is how long this agent has not heard a host: since its last
// beat, or since the agent started if it heard none.
func (h *Heard) Silence(host state.HostID) time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	last, ok := h.last[host]
	if !ok {
		last = h.started
	}
	return h.now().Sub(last)
}

// majority tells whether count of total voters are a majority.
func majority(count, total int) bool {
	return count > total/2
}
