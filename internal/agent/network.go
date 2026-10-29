package agent

import (
	"context"
	"io"
	"log/slog"
	"net/netip"
	"time"

	"github.com/webcodingcat/clowder/internal/network"
	"github.com/webcodingcat/clowder/internal/raftnode"
	"github.com/webcodingcat/clowder/internal/state"
)

// HostNetwork is what the agent changes in the host's network; network.Host
// is the real one, tests use a fake.
type HostNetwork interface {
	SetUpInterface(ctx context.Context, nodeAddress netip.Addr) error
	ApplyRules(ctx context.Context, ruleset network.Ruleset) error
	StartResolver() (io.Closer, error)
}

// reconcileNetwork keeps the nft rules as the state says until ctx ends:
// after a change of the state when the rules differ, and every
// reconcileInterval anyway, to repair what someone changed by hand.
func reconcileNetwork(ctx context.Context, node *raftnode.Node, hostNetwork HostNetwork, logger *slog.Logger) {
	// applied is the ruleset that is loaded; nil means "load it again"
	// (at the start, after a failure, at each repair)
	var applied *network.Ruleset
	for {
		// take the channel before reading, so no change between goes unseen
		changed := node.Changed()
		wanted := wantedRuleset(node)
		if applied == nil || *applied != wanted {
			applied = nil
			if err := hostNetwork.ApplyRules(ctx, wanted); err != nil {
				logger.Error("apply the nft rules", "error", err)
			} else {
				applied = &wanted
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-changed:
		case <-time.After(reconcileInterval):
			applied = nil
		}
	}
}

// wantedRuleset is the nft ruleset for the current state.
func wantedRuleset(node *raftnode.Node) network.Ruleset {
	var ruleset network.Ruleset
	node.Read(func(current *state.State) {
		// empty before the first cluster apply; clusterdir.Check lets no
		// other invalid address into the state
		if address, err := netip.ParseAddr(current.Config.Spec.Ingress.FloatingIP); err == nil {
			ruleset.FloatingIP = address
		}
	})
	return ruleset
}
