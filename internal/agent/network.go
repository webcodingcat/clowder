package agent

import (
	"context"
	"io"
	"log/slog"
	"net/netip"
	"time"

	"github.com/webcodingcat/clowder/internal/network"
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
func reconcileNetwork(ctx context.Context, cluster stateReader, hostNetwork HostNetwork, logger *slog.Logger) {
	// applied is the ruleset loaded at appliedAt; nil at the start and
	// after a failure
	var applied *network.Ruleset
	var appliedAt time.Time
	runPasses(ctx, cluster, nil, func() {
		wanted := wantedRuleset(cluster)
		repairDue := time.Since(appliedAt) >= reconcileInterval
		if applied != nil && *applied == wanted && !repairDue {
			return
		}
		applied = nil
		if err := hostNetwork.ApplyRules(ctx, wanted); err != nil {
			logger.Error("apply the nft rules", "error", err)
			return
		}
		applied, appliedAt = &wanted, time.Now()
	})
}

// wantedRuleset is the nft ruleset for the current state.
func wantedRuleset(cluster stateReader) network.Ruleset {
	var ruleset network.Ruleset
	cluster.Read(func(current *state.State) {
		// empty before the first cluster apply; clusterspec.Check lets no
		// other invalid address into the state
		if address, err := netip.ParseAddr(current.Config.Spec.Ingress.FloatingIP); err == nil {
			ruleset.FloatingIP = address
		}
	})
	return ruleset
}
