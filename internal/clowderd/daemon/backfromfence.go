package daemon

import (
	"context"
	"log/slog"

	"github.com/webcodingcat/clowder/internal/clowderd/daemon/loop"
	"github.com/webcodingcat/clowder/internal/controller"
	"github.com/webcodingcat/clowder/internal/state"
)

// backFromFenceReporter reports a fenced host recovering once it runs, and
// active once its copies are synced. Only the host can report either step;
// the leader only knows that it powered the server on.
type backFromFenceReporter struct {
	cluster loop.Cluster
	self    state.HostID
	propose controller.Propose
	logger  *slog.Logger
}

// Run runs after the state has caught up, so the host knows that its
// stacks moved before it says it is back.
func (r backFromFenceReporter) Run(ctx context.Context) {
	loop.Run(ctx, r.cluster, nil, func() {
		var next, from state.HostPhase
		r.cluster.Read(func(current *state.State) {
			host, ok := current.Hosts[r.self]
			switch {
			case !ok || current.HostLocked(r.self):
				// the fence that holds it is not over yet
			case host.Phase == state.Fenced:
				next = state.Recovering
			case host.Phase == state.Recovering && current.CaughtUp(r.self):
				next = state.Active
			}
			if ok {
				from = host.Phase
			}
		})
		if next == "" {
			return
		}
		err := loop.CommitAsHost(ctx, r.propose, r.self, state.KindSetHostPhase,
			state.SetHostPhase{Host: r.self, Phase: next, From: from})
		if err != nil {
			r.logger.Warn("report this host back", "phase", next, "error", err)
			return
		}
		r.logger.Info("this host is back after it was fenced", "phase", next)
	})
}
