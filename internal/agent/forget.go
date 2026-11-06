package agent

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"

	"github.com/webcodingcat/clowder/internal/agent/loop"
	"github.com/webcodingcat/clowder/internal/controller"
	"github.com/webcodingcat/clowder/internal/state"
	"github.com/webcodingcat/clowder/internal/storage"
)

// forgetPeers runs until ctx ends. The state lists the removed hosts that
// this host still has to forget as DRBD peers; forgetPeers forgets each one
// and reports it (report-peer-forgotten). A removed host's ID is free again
// once every host has forgotten it.
func forgetPeers(ctx context.Context, cluster loop.Cluster, self state.HostID, driver Storage,
	propose controller.Propose, logger *slog.Logger,
) {
	loop.Run(ctx, cluster, nil, func() {
		var peers []state.HostID
		cluster.Read(func(current *state.State) {
			for _, peer := range slices.Sorted(maps.Keys(current.Forgetting)) {
				if slices.Contains(current.Forgetting[peer], self) {
					peers = append(peers, peer)
				}
			}
		})
		for _, peer := range peers {
			err := driver.ForgetPeer(ctx, int(peer))
			switch {
			case errors.Is(err, storage.ErrNotYet):
				logger.Debug("a removed host is not forgotten yet", "peer", peer, "why", err)
				continue
			case err != nil:
				logger.Warn("forget a removed host as a DRBD peer", "peer", peer, "error", err)
				continue
			}
			report := state.ReportPeerForgotten{Host: self, Peer: peer}
			if err := loop.CommitAsHost(ctx, propose, self, state.KindReportPeerForgotten, report); err != nil {
				logger.Warn("report a forgotten DRBD peer", "peer", peer, "error", err)
			}
		}
	})
}
