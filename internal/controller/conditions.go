package controller

import (
	"maps"
	"sync"
	"time"
)

// Conditions holds what the leader's loops see but the state does not show,
// each with the time it began. It lives in memory only, so after a change of
// leader an alert comes a little later, but never wrongly.
type Conditions struct {
	mu    sync.Mutex
	since map[string]time.Time
	// seen holds the conditions that a loop has looked at since this agent
	// began to lead, whether they hold or not
	seen map[string]bool
}

// NewConditions returns a Conditions without any condition.
func NewConditions() *Conditions {
	return &Conditions{since: map[string]time.Time{}, seen: map[string]bool{}}
}

// Observe records whether a condition holds now. It reports whether the
// condition began with this call, so that a loop logs a condition only once.
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

// Since reports whether a condition holds and since when.
func (c *Conditions) Since(key string) (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	since, ok := c.since[key]
	return since, ok
}

// Seen reports whether a loop has looked at a condition since this agent
// began to lead.
func (c *Conditions) Seen(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.seen[key]
}

// Keep forgets every condition except those in keys. Without it, the
// conditions of stacks and hosts that are gone would stay in memory for as
// long as this agent leads.
func (c *Conditions) Keep(keys map[string]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	maps.DeleteFunc(c.since, func(key string, _ time.Time) bool { return !keys[key] })
	maps.DeleteFunc(c.seen, func(key string, _ bool) bool { return !keys[key] })
}

// Reset forgets every condition. It is called when this agent stops
// leading, because what it saw will be stale if it leads again later.
func (c *Conditions) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	clear(c.since)
	clear(c.seen)
}
