package agent

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"reflect"
	"slices"
	"time"

	"github.com/webcodingcat/clowder/internal/agent/loop"
	"github.com/webcodingcat/clowder/internal/atomicfile"
	"github.com/webcodingcat/clowder/internal/network"
	"github.com/webcodingcat/clowder/internal/state"
)

// HostNetwork is what the agent changes in the host's network (network.Host,
// or a fake in tests).
type HostNetwork interface {
	SetUpInterface(ctx context.Context, nodeAddress netip.Addr) error
	ApplyMesh(ctx context.Context, mesh network.Mesh) error
	ApplyRules(ctx context.Context, ruleset network.Ruleset) error
	AllowForwards(ctx context.Context) error
	ForgetForwards(ctx context.Context, floatingIP netip.Addr, forwards []network.Forward) error
	StartResolver() (io.Closer, error)
}

type wantedNetwork struct {
	mesh    network.Mesh
	ruleset network.Ruleset
}

// reconcileNetwork applies the network every loop.Interval even when it
// has not changed, to repair what someone changed by hand. It keeps the
// applied mesh in net.json for restoreMesh.
func reconcileNetwork(ctx context.Context, cluster loop.Cluster, files Files, self state.HostID,
	hostNetwork HostNetwork, logger *slog.Logger,
) {
	// applied is what was applied at appliedAt; nil at the start and after
	// a failure
	var applied *wantedNetwork
	var appliedAt time.Time
	// forwarded holds the forwards of the last loaded rules; nil until the
	// first load. The first load forgets no connections: a port that moved
	// while the agent was down keeps its connections until they end.
	var forwarded []network.Forward
	// dockerAllowed says that Docker's chain has the rule for the forwarded
	// ports (AllowForwards). It is tracked separately from the rules and
	// the mesh, which must not wait for Docker to come up.
	dockerAllowed := false
	loop.Run(ctx, cluster, nil, func() {
		wanted, known := readWantedNetwork(cluster, self)
		if !known {
			// a joined host's state is empty until the leader's log reaches
			// it over the mesh of net.json. A mesh built from that empty
			// state would have no peers and cut the host off for good.
			return
		}
		repairDue := time.Since(appliedAt) >= loop.Interval
		if !dockerAllowed || repairDue {
			err := hostNetwork.AllowForwards(ctx)
			if err != nil {
				logger.Error("let the forwarded ports pass Docker's rules", "error", err)
			}
			dockerAllowed = err == nil
		}
		// DeepEqual covers every field, including fields added later;
		// readWantedNetwork sorts the lists
		if applied != nil && reflect.DeepEqual(*applied, wanted) && !repairDue {
			return
		}
		applied = nil
		// the rules come first: the containers' isolation must not wait for
		// a mesh that fails
		if err := hostNetwork.ApplyRules(ctx, wanted.ruleset); err != nil {
			logger.Error("apply the nft rules", "error", err)
			return
		}
		if forwarded != nil {
			moved := movedForwards(forwarded, wanted.ruleset.Forwards)
			if err := hostNetwork.ForgetForwards(ctx, wanted.ruleset.FloatingIP, moved); err != nil {
				logger.Error("forget the connections of moved ports", "error", err)
				return
			}
		}
		forwarded = append([]network.Forward{}, wanted.ruleset.Forwards...)
		if err := hostNetwork.ApplyMesh(ctx, wanted.mesh); err != nil {
			logger.Error("apply the WireGuard mesh", "error", err)
			return
		}
		if err := files.writeMesh(wanted.mesh); err != nil {
			logger.Error("keep the WireGuard mesh for the next start", "error", err)
			return
		}
		applied, appliedAt = &wanted, time.Now()
	})
}

func (f Files) writeMesh(mesh network.Mesh) error {
	content, err := json.MarshalIndent(mesh, "", "  ")
	if err != nil {
		return fmt.Errorf("write %s: %w", f.meshFile(), err)
	}
	content = append(content, '\n')
	if old, err := os.ReadFile(f.meshFile()); err == nil && bytes.Equal(old, content) {
		return nil
	}
	return atomicfile.Write(f.meshFile(), content, 0o600)
}

// restoreMesh applies the mesh of net.json, if there is one. Raft runs
// over the mesh, so without the file a rebooted host would have to learn
// its peers from a leader that it cannot reach without them.
func restoreMesh(ctx context.Context, files Files, self state.HostID, hostNetwork HostNetwork) error {
	content, err := os.ReadFile(files.meshFile())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", files.meshFile(), err)
	}
	var mesh network.Mesh
	if err := json.Unmarshal(content, &mesh); err != nil {
		return fmt.Errorf("read %s: %w", files.meshFile(), err)
	}
	if mesh.Self != self {
		return fmt.Errorf("%s is the mesh of host %d, not of this host %d", files.meshFile(), mesh.Self, self)
	}
	if err := hostNetwork.ApplyMesh(ctx, mesh); err != nil {
		return fmt.Errorf("restore the WireGuard mesh of %s: %w", files.meshFile(), err)
	}
	return nil
}

// readWantedNetwork keeps the addresses of a removed host closed to
// containers, because it may still run until its server is gone. known is
// false while the state does not know this host yet; such a state says
// nothing about the network.
func readWantedNetwork(cluster loop.Cluster, self state.HostID) (wanted wantedNetwork, known bool) {
	wanted = wantedNetwork{mesh: network.Mesh{Self: self}, ruleset: network.Ruleset{NodeAddress: network.NodeAddress(self)}}
	ruleset := &wanted.ruleset
	cluster.Read(func(current *state.State) {
		if _, known = current.Hosts[self]; !known {
			return
		}
		spec := current.Config.Spec
		// empty before the first configuration; clusterspec.Check lets no
		// other invalid value into the state
		if address, err := netip.ParseAddr(spec.Ingress.FloatingIP); err == nil {
			ruleset.FloatingIP = address
		}
		if prefix, err := netip.ParsePrefix(spec.PrivateNetwork); err == nil {
			ruleset.PrivateNetwork = prefix
		}
		for _, host := range current.Hosts {
			for _, address := range []netip.Addr{host.Underlay, host.PublicAddress} {
				// the state accepts only IPv4; anything else would break the
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
		if ruleset.FloatingIP.IsValid() {
			ruleset.Forwards = forwardsOf(current, self)
		}
	})
	slices.SortFunc(ruleset.ClusterHosts, netip.Addr.Compare)
	slices.SortFunc(wanted.mesh.Peers, func(a, b network.Peer) int { return cmp.Compare(a.ID, b.ID) })
	slices.Sort(ruleset.Peers)
	return wanted, known
}

// forwardsOf gives a stack without a host no forwards: its ports lead
// nowhere until it is placed.
func forwardsOf(current *state.State, self state.HostID) []network.Forward {
	var forwards []network.Forward
	for _, stack := range current.Stacks {
		if stack.Host == nil || *stack.Host == self {
			continue
		}
		for _, service := range stack.Spec.Services {
			// the state's specs are normalized: protocol and published port
			// are filled in
			for _, port := range service.Ports {
				forwards = append(forwards, network.Forward{
					Protocol: port.Protocol, Port: port.Published, Target: network.NodeAddress(*stack.Host),
				})
			}
		}
	}
	network.SortForwards(forwards)
	return forwards
}

// movedForwards counts a port that is newly forwarded or no longer
// forwarded as moved too, as when a stack comes to this host or leaves it.
func movedForwards(old, current []network.Forward) []network.Forward {
	type port struct {
		protocol string
		number   uint16
	}
	targets := map[port]netip.Addr{}
	for _, forward := range old {
		targets[port{forward.Protocol, forward.Port}] = forward.Target
	}
	var moved []network.Forward
	for _, forward := range current {
		key := port{forward.Protocol, forward.Port}
		if target, ok := targets[key]; !ok || target != forward.Target {
			moved = append(moved, forward)
		}
		delete(targets, key)
	}
	for key, target := range targets {
		moved = append(moved, network.Forward{Protocol: key.protocol, Port: key.number, Target: target})
	}
	network.SortForwards(moved)
	return moved
}
