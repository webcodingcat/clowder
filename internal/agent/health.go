package agent

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/webcodingcat/clowder/internal/controller"
	"github.com/webcodingcat/clowder/internal/raftnode"
	"github.com/webcodingcat/clowder/internal/state"
)

// beatTimeout bounds one beat. A voter that does not take its beat in time
// holds up no other voter, and the next beat follows anyway.
const beatTimeout = 2 * time.Second

// Beater sends this host's beat to a voter (api.Server.Beat).
type Beater func(ctx context.Context, voter state.HostID) error

// sendBeats tells every voter that this host is alive, every
// controller.BeatInterval, until ctx ends. The leader considers a host
// failed when a majority of the voters has not heard from it for
// controller.FailAfter. It beats from the start, before the state has
// caught up, because the beat says that the agent runs, not what it runs.
// A voter that does not answer is logged only at debug level; while the
// voter is down, every beat would log.
func sendBeats(ctx context.Context, node *raftnode.Node, self state.HostID, beat Beater, logger *slog.Logger) {
	for {
		var voters []state.HostID
		node.Read(func(current *state.State) {
			for _, id := range slices.Sorted(maps.Keys(current.Hosts)) {
				if host := current.Hosts[id]; host.Voter && host.Phase != state.Removed && id != self {
					voters = append(voters, id)
				}
			}
		})
		var sent sync.WaitGroup
		for _, voter := range voters {
			sent.Go(func() {
				beatCtx, cancel := context.WithTimeout(ctx, beatTimeout)
				defer cancel()
				if err := beat(beatCtx, voter); err != nil {
					logger.Debug("a voter did not take this host's beat", "voter", voter, "error", err)
				}
			})
		}
		sent.Wait()
		select {
		case <-ctx.Done():
			return
		case <-time.After(controller.BeatInterval):
		}
	}
}
