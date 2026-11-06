package agent

import (
	"context"
	"log/slog"

	"github.com/webcodingcat/clowder/internal/agent/loop"
	"github.com/webcodingcat/clowder/internal/controller"
	"github.com/webcodingcat/clowder/internal/state"
)

// reportRecovery tells the cluster how far this host has come back after
// it was fenced, until ctx ends. Once the host runs again, it becomes
// recovering. Once its copies have caught up with their peers (the volume
// reconciler reports each one synced), it becomes active and takes stacks.
// Only the host can report either step. The leader only knows that it
// powered the server on. A step is reported only after the state has
// caught up, so the host knows that its stacks moved before it says it is
// back.
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
