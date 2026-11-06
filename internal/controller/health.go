package controller

import (
	"sync"
	"time"

	"github.com/webcodingcat/clowder/internal/state"
)

// How hosts say that they are alive, and when the leader considers one
// failed. The beats go to every voter, not through the Raft log. A new
// leader has heard them too, so the time that a host has been silent keeps
// counting across a change of leader.
const (
	// BeatInterval is how often an agent tells every voter it is alive.
	BeatInterval = 5 * time.Second
	// FailAfter is how long a majority of the voters must not have heard
	// from a host before it is failed. It is well above a pause of the
	// network or of all the hosts (20 s), and below the time that systemd
	// takes to restart a hung agent (WatchdogSec) plus the agent's start.
	FailAfter = 45 * time.Second
	// backWithin is how recently a majority of the voters must have heard
	// from a failed host for it to become active again. It is a few beats,
	// so a single beat that slips through a flaky network does not bring
	// the host back.
	backWithin = 3 * BeatInterval
)

// Heard records when this agent last heard each host's beat. It is kept in
// memory only, so after a start it counts from the start. A host that this
// agent has not heard yet may well be alive, and the other voters hold the
// earlier beats.
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

// Silence is how long this agent has not heard from a host: the time since
// its last beat, or since the agent started if it heard no beat.
func (h *Heard) Silence(host state.HostID) time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	last, ok := h.last[host]
	if !ok {
		last = h.started
	}
	return h.now().Sub(last)
}

// majority tells whether count voters out of total voters are a majority.
func majority(count, total int) bool {
	return count > total/2
}
