package controller

import (
	"sync"
	"time"
)

// Conditions are what the leader's loops see that the state does not show
// (the Floating IP not held, a host over capacity, Docker not answering for
// a while), each with the time it began. They are in memory: after a change
// of leader they start again, so a new alert comes a little later rather
// than wrongly, and one that is open stays until the loop that observes its
// condition has looked.
type Conditions struct {
	mu    sync.Mutex
	since map[string]time.Time
	// seen are the conditions a loop has looked at since this agent began
	// to lead, holding or not
	seen map[string]bool
}

// NewConditions has no condition yet.
func NewConditions() *Conditions {
	return &Conditions{since: map[string]time.Time{}, seen: map[string]bool{}}
}

// Observe records whether a condition holds now. It tells whether it began
// with this call, so a loop logs a condition once.
func (c *Conditions) Observe(key string, holds bool) (began bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen[key] = true
	_, held := c.since[key]
	switch {
	case holds && !held:
		c.since[key] = time.Now()
		return true
	case !holds:
		delete(c.since, key)
	}
	return false
}

// Since tells since when a condition holds, and whether it does.
func (c *Conditions) Since(key string) (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	since, ok := c.since[key]
	return since, ok
}

// Seen tells whether a loop has looked at a condition since this agent
// began to lead.
func (c *Conditions) Seen(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.seen[key]
}

// Reset forgets every condition: when this agent leads no more, what it
// saw is stale by the time it might lead again.
func (c *Conditions) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	clear(c.since)
	clear(c.seen)
}
