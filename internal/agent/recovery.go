package agent

import (
	"context"
	"log/slog"

	"github.com/webcodingcat/clowder/internal/agent/loop"
	"github.com/webcodingcat/clowder/internal/controller"
	"github.com/webcodingcat/clowder/internal/state"
)

// reportRecovery reports a fenced host recovering once it runs, and active
// once its copies are synced. Only the host can report either step; the
// leader only knows that it powered the server on. It runs after the state
// has caught up, so the host knows that its stacks moved before it says it
// is back.
func reportRecovery(ctx context.Context, cluster loop.Cluster, self state.HostID, propose controller.Propose,
	logger *slog.Logger,
) {
	loop.Run(ctx, cluster, nil, func() {
		var next, from state.HostPhase
		cluster.Read(func(current *state.State) {
			host, ok := current.Hosts[self]
			switch {
			case !ok || current.HostLocked(self):
				// the fence that holds it is not over yet
			case host.Phase == state.Fenced:
				next = state.Recovering
			case host.Phase == state.Recovering && current.CaughtUp(self):
				next = state.Active
			}
			if ok {
				from = host.Phase
			}
		})
		if next == "" {
			return
		}
		err := loop.CommitAsHost(ctx, propose, self, state.KindSetHostPhase,
			state.SetHostPhase{Host: self, Phase: next, From: from})
		if err != nil {
			logger.Warn("report this host back", "phase", next, "error", err)
			return
		}
		logger.Info("this host is back after it was fenced", "phase", next)
	})
}
