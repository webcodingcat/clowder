package daemon

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/webcodingcat/clowder/internal/controller"
	"github.com/webcodingcat/clowder/internal/passes"
	"github.com/webcodingcat/clowder/internal/raftnode"
	"github.com/webcodingcat/clowder/internal/state"
)

// beatTimeout limits one beat. A voter that does not accept its beat in
// time does not hold up the other voters, and the next beat follows anyway.
const beatTimeout = 2 * time.Second

// Beater sends this host's beat to a voter (api.Peers.Beat).
type Beater func(ctx context.Context, voter state.HostID) error

// beatSender tells every voter that this host is alive; the leader fails a
// host that a majority of the voters has not heard from.
type beatSender struct {
	node   *raftnode.Node
	self   state.HostID
	beat   Beater
	logger *slog.Logger
}

// Run starts before the state has caught up, because a beat says that the
// agent runs, not what it runs. A voter that does not answer is logged at
// debug level only, or every beat would log while the voter is down.
func (s beatSender) Run(ctx context.Context) {
	passes.Run(ctx, nil, nil, func() time.Duration {
		var voters []state.HostID
		s.node.Read(func(current *state.State) {
			for _, id := range slices.Sorted(maps.Keys(current.Hosts)) {
				if host := current.Hosts[id]; host.Voter && host.Phase != state.Removed && id != s.self {
					voters = append(voters, id)
				}
			}
		})
		var sent sync.WaitGroup
		for _, voter := range voters {
			sent.Go(func() {
				beatCtx, cancel := context.WithTimeout(ctx, beatTimeout)
				defer cancel()
				if err := s.beat(beatCtx, voter); err != nil {
					s.logger.Debug("a voter did not take this host's beat", "voter", voter, "error", err)
				}
			})
		}
		sent.Wait()
		return controller.BeatInterval
	})
}
