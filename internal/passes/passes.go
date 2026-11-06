// Package passes runs the loops of the agent and of the leader: each loop
// runs a pass, then waits for a change of the state, a wake-up or its next
// pass's time, and runs the next pass.
package passes

import (
	"context"
	"time"
)

// Interval is how long a loop waits for its next pass when the state does
// not change. These passes repair what someone changed by hand, and they
// retry what failed.
const Interval = 30 * time.Second

// Run runs pass at once. After that, it runs pass again whenever the state
// changes, wake fires, or the wait that the last pass returned has passed,
// until ctx ends. A wait of 0 runs the next pass at once. changed (the
// state's Changed) and wake may be nil for a loop that only keeps time.
func Run(ctx context.Context, changed func() <-chan struct{}, wake <-chan struct{}, pass func() time.Duration) {
	for {
		// get the channel before the pass, so no change during it is missed
		var stateChanged <-chan struct{}
		if changed != nil {
			stateChanged = changed()
		}
		wait := pass()
		select {
		case <-ctx.Done():
			return
		case <-stateChanged:
		case <-wake:
		case <-time.After(wait):
		}
	}
}

// Soonest is the wait until the next pass when something falls due after
// due: due if it comes before Interval, else Interval. A due of 0 or less
// means nothing falls due.
func Soonest(due time.Duration) time.Duration {
	if due > 0 {
		return min(due, Interval)
	}
	return Interval
}
