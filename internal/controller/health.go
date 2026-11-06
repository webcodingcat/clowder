package controller

import (
	"sync"
	"time"

	"github.com/webcodingcat/clowder/internal/state"
)

// How hosts report that they are alive, and when the leader considers a
// host failed. The beats go to every voter, not through the Raft log. A new
// leader has heard them too, so a host's silence keeps counting across a
// change of leader.
const (
	// BeatInterval is how often an agent tells every voter that it is alive.
	BeatInterval = 5 * time.Second
	// FailAfter is how long a majority of the voters must not have heard
	// from a host before the host is failed. It is well above a 20 s pause
	// of the network or of all hosts. It is below the time that systemd
	// takes to restart a hung agent (WatchdogSec) plus the agent's start.
	FailAfter = 45 * time.Second
	// backWithin is how recently a majority of the voters must have heard
	// from a failed host for it to become active again. It spans a few
	// beats, so a single beat that gets through a flaky network does not
	// bring the host back.
	backWithin = 3 * BeatInterval
)

// Heard records when this agent last heard each host's beat. It lives in
// memory only, so after this agent starts, it counts from its own start.
// A host that this agent has not heard yet may well be alive; the other
// voters hold its earlier beats.
type Heard struct {
	mu      sync.Mutex
	started time.Time
	last    map[state.HostID]time.Time
	// now is time.Now; tests replace it
	now func() time.Time
}

// NewHeard returns a Heard that has heard no host yet.
func NewHeard() *Heard {
	return &Heard{started: time.Now(), last: map[state.HostID]time.Time{}, now: time.Now}
}

// Beat records that a host is alive. The API's beats endpoint calls it.
func (h *Heard) Beat(host state.HostID) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.last[host] = h.now()
}

// Silence returns how long this agent has not heard from a host: the time
// since its last beat, or since the agent started if it heard no beat.
func (h *Heard) Silence(host state.HostID) time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	last, ok := h.last[host]
	if !ok {
		last = h.started
	}
	return h.now().Sub(last)
}

// majority reports whether count voters out of total voters are a majority.
func majority(count, total int) bool {
	return count > total/2
}
