package agent

import (
	"cmp"
	"context"
	"io"
	"log/slog"
	"net/netip"
	"reflect"
	"slices"
	"time"

	"github.com/webcodingcat/clowder/internal/agent/loop"
	"github.com/webcodingcat/clowder/internal/network"
	"github.com/webcodingcat/clowder/internal/state"
)

// HostNetwork is what the agent changes in the host's network; network.Host
// is the real one, tests use a fake.
type HostNetwork interface {
	SetUpInterface(ctx context.Context, nodeAddress netip.Addr) error
	ApplyMesh(ctx context.Context, mesh network.Mesh) error
	ApplyRules(ctx context.Context, ruleset network.Ruleset) error
	StartResolver() (io.Closer, error)
}

// wantedNetwork is what the state wants of the host's network.
type wantedNetwork struct {
	mesh    network.Mesh
	ruleset network.Ruleset
}

// reconcileNetwork keeps the WireGuard mesh and the nft rules as the state
// says until ctx ends: after a change of the state when they differ, and
// every loop.Interval anyway, to repair what someone changed by hand.
func reconcileNetwork(ctx context.Context, cluster loop.Cluster, self state.HostID, hostNetwork HostNetwork,
	logger *slog.Logger,
) {
	// applied is what was applied at appliedAt; nil at the start and after
	// a failure
	var applied *wantedNetwork
	var appliedAt time.Time
	loop.Run(ctx, cluster, nil, func() {
		wanted := readWantedNetwork(cluster, self)
		repairDue := time.Since(appliedAt) >= loop.Interval
		// DeepEqual covers every field, a new one too; readWantedNetwork
		// sorts the lists
		if applied != nil && reflect.DeepEqual(*applied, wanted) && !repairDue {
			return
		}
		applied = nil
		// the rules first: the containers' boundaries must not wait for a
		// mesh that fails
		if err := hostNetwork.ApplyRules(ctx, wanted.ruleset); err != nil {
			logger.Error("apply the nft rules", "error", err)
			return
		}
		if err := hostNetwork.ApplyMesh(ctx, wanted.mesh); err != nil {
			logger.Error("apply the WireGuard mesh", "error", err)
			return
		}
		applied, appliedAt = &wanted, time.Now()
	})
}

// readWantedNetwork reads the mesh and the nft ruleset from the current
// state. Every other host is a peer until it is removed. The addresses of
// all hosts stay closed to containers, a removed host's too: it may still
// run, until the hoster's server is gone.
func readWantedNetwork(cluster loop.Cluster, self state.HostID) wantedNetwork {
	wanted := wantedNetwork{mesh: network.Mesh{Self: self}}
	ruleset := &wanted.ruleset
	cluster.Read(func(current *state.State) {
		spec := current.Config.Spec
		// empty before the first cluster apply; clusterspec.Check lets no
		// other invalid value into the state
		if address, err := netip.ParseAddr(spec.Ingress.FloatingIP); err == nil {
			ruleset.FloatingIP = address
		}
		if prefix, err := netip.ParsePrefix(spec.PrivateNetwork); err == nil {
			ruleset.PrivateNetwork = prefix
		}
		for _, host := range current.Hosts {
			for _, address := range []netip.Addr{host.Underlay, host.PublicAddress} {
				// the state takes only IPv4; anything else would break the
				// whole table (its set is ipv4_addr)
				if address.Is4() && !slices.Contains(ruleset.ClusterHosts, address) {
					ruleset.ClusterHosts = append(ruleset.ClusterHosts, address)
				}
			}
			if host.ID != self && host.Phase != state.Removed {
				wanted.mesh.Peers = append(wanted.mesh.Peers,
					network.Peer{ID: host.ID, PublicKey: host.WGPublicKey, Underlay: host.Underlay})
				ruleset.Peers = append(ruleset.Peers, host.ID)
			}
		}
	})
	slices.SortFunc(ruleset.ClusterHosts, netip.Addr.Compare)
	slices.SortFunc(wanted.mesh.Peers, func(a, b network.Peer) int { return cmp.Compare(a.ID, b.ID) })
	slices.Sort(ruleset.Peers)
	return wanted
}
