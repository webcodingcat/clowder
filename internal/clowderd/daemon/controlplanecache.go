package daemon

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// askAgainAfter keeps a host from asking the control plane at every pass
// while it does not answer. Traefik's loop runs a pass at every Docker
// event, and each failed call would hold it up for the API's timeout.
const askAgainAfter = 10 * time.Second

// controlPlaneCache keeps in memory what the control plane gave this host:
// the secrets of the stacks it runs, or the certificates' keys while it
// holds the Floating IP (api.Peers.SecretsOfThisHost,
// CertificatesOfThisHost). It asks again only when the state names another
// version of a value (a secret's digest, a certificate's chain), as the
// kubelet keeps the secrets of its pods. Each answer replaces all that is
// kept, so a value that the host may no longer read is gone after the next
// answer, or at forget. On a control-plane host the API server opens the
// values itself, so it asks nobody.
//
// Only a failed call pauses the asking. An answer without the value cannot
// repeat for long: the leader answers from a state at least as new as this
// host's, so the value comes as soon as the leader's state gives it, and
// this host asks again only at its own next change.
type controlPlaneCache[T any] struct {
	// what names a value in errors ("secret", "certificate")
	what    string
	fetch   func(ctx context.Context) (map[string]T, error)
	version func(T) string

	// mu is held while fetch runs, so that the jobs that miss a value at
	// once share one call
	mu       sync.Mutex
	known    map[string]T
	failure  error
	failedAt time.Time
}

// get returns the value of key in the given version. It holds mu during
// the call to the control plane, so jobs that ask at once share one call;
// the cost is that they wait for it, up to the call's budget, and that
// after a failure every key waits askAgainAfter.
func (c *controlPlaneCache[T]) get(ctx context.Context, key, version string) (T, error) {
	var none T
	c.mu.Lock()
	defer c.mu.Unlock()
	if value, ok := c.known[key]; ok && c.version(value) == version {
		return value, nil
	}
	if c.failure != nil && time.Since(c.failedAt) < askAgainAfter {
		return none, c.failure
	}
	fetched, err := c.fetch(ctx)
	if err != nil {
		c.failure, c.failedAt = err, time.Now()
		return none, err
	}
	c.known, c.failure = fetched, nil
	value, ok := fetched[key]
	switch {
	case !ok:
		return none, fmt.Errorf("the control plane gives this host no %s %s (its state may not place it here yet)",
			c.what, key)
	case c.version(value) != version:
		return none, fmt.Errorf("the control plane has another version of %s %s; the next pass tries again", c.what, key)
	}
	return value, nil
}

// forget drops all that is kept, once the host may read none of it.
func (c *controlPlaneCache[T]) forget() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.known = nil
}
