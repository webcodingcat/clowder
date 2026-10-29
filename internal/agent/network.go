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

// reconcileInterval is how often the reconcilers run without a change of the
// state, to repair what someone changed by hand.
const reconcileInterval = 30 * time.Second

// reconcileNetwork keeps the nft rules as the state says until ctx ends:
// after a change of the state when the rules differ, and every
// reconcileInterval anyway, to repair what someone changed by hand.
func reconcileNetwork(ctx context.Context, node *raftnode.Node, hostNetwork HostNetwork, logger *slog.Logger) {
	var applied network.Ruleset
	upToDate, repair := false, true
	for {
		changed := node.Changed()
		wanted := wantedRuleset(node)
		if repair || !upToDate || wanted != applied {
			if err := hostNetwork.ApplyRules(ctx, wanted); err != nil {
				logger.Error("apply the nft rules", "error", err)
				upToDate = false
			} else {
				applied, upToDate = wanted, true
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-changed:
			repair = false
		case <-time.After(reconcileInterval):
			repair = true
		}
	}
}

// wantedRuleset is the nft ruleset for the current state.
func wantedRuleset(node *raftnode.Node) network.Ruleset {
	var ruleset network.Ruleset
	node.Read(func(current *state.State) {
		if address, err := netip.ParseAddr(current.Config.Spec.Ingress.FloatingIP); err == nil {
			ruleset.FloatingIP = address
		}
	})
	return ruleset
}
