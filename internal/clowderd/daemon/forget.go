package daemon

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"

	"github.com/webcodingcat/clowder/internal/clowderd/daemon/loop"
	"github.com/webcodingcat/clowder/internal/controller"
	"github.com/webcodingcat/clowder/internal/state"
	"github.com/webcodingcat/clowder/internal/storage"
)

// peerForgetter forgets the removed hosts that the state lists for this
// host as DRBD peers, and reports each one. A removed host's ID becomes
// free again once every host has reported it.
type peerForgetter struct {
	cluster loop.Cluster
	self    state.HostID
	storage Storage
	propose controller.Propose
	logger  *slog.Logger
}

// Run forgets the peers at every pass until ctx ends.
func (f peerForgetter) Run(ctx context.Context) {
	loop.Run(ctx, f.cluster, nil, func() {
		var peers []state.HostID
		f.cluster.Read(func(current *state.State) {
			for _, peer := range slices.Sorted(maps.Keys(current.Forgetting)) {
				if slices.Contains(current.Forgetting[peer], f.self) {
					peers = append(peers, peer)
				}
			}
		})
		for _, peer := range peers {
			err := f.storage.ForgetPeer(ctx, int(peer))
			switch {
			case errors.Is(err, storage.ErrNotYet):
				f.logger.Debug("a removed host is not forgotten yet", "peer", peer, "why", err)
				continue
			case err != nil:
				f.logger.Warn("forget a removed host as a DRBD peer", "peer", peer, "error", err)
				continue
			}
			report := state.ReportPeerForgotten{Host: f.self, Peer: peer}
			if err := loop.CommitAsHost(ctx, f.propose, f.self, state.KindReportPeerForgotten, report); err != nil {
				f.logger.Warn("report a forgotten DRBD peer", "peer", peer, "error", err)
			}
		}
	})
}
