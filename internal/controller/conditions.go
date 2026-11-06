package controller

import (
	"maps"
	"sync"
	"time"
)

// Conditions are what the leader's loops see but the state does not show,
// such as the Floating IP not being held, a host over capacity, or Docker
// not answering for a while. Each has the time it began. They are kept in
// memory only, so they start again after a change of leader. A new alert
// then comes a little later rather than wrongly, and an open alert stays
// open until the loop that observes its condition has looked.
type Conditions struct {
	mu    sync.Mutex
	since map[string]time.Time
	// seen are the conditions that a loop has looked at since this agent
	// began to lead, whether they hold or not
	seen map[string]bool
}

// NewConditions has no condition yet.
func NewConditions() *Conditions {
	return &Conditions{since: map[string]time.Time{}, seen: map[string]bool{}}
}

// Observe records whether a condition holds now. It tells whether the
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

// Since tells whether a condition holds and since when.
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

// Keep forgets every condition except those in keys. Otherwise the
// conditions of stacks and hosts that are gone would stay in memory as long
// as this agent leads.
func (c *Conditions) Keep(keys map[string]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	maps.DeleteFunc(c.since, func(key string, _ time.Time) bool { return !keys[key] })
	maps.DeleteFunc(c.seen, func(key string, _ bool) bool { return !keys[key] })
}

// Reset forgets every condition. Once this agent no longer leads, what it
// saw is stale by the time it might lead again.
func (c *Conditions) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	clear(c.since)
	clear(c.seen)
}
