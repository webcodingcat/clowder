package network

import (
	"cmp"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/webcodingcat/clowder/internal/clusterspec"
	"github.com/webcodingcat/clowder/internal/state"
)

// Fixed addresses of clowder's network.
var (
	// Resolver is the DNS resolver that every container uses. Its address
	// is on clowder0.
	Resolver = netip.MustParseAddr("169.254.77.53")
	// Overlay holds the node addresses.
	Overlay = clusterspec.Overlay
	// metadataService is Hetzner's metadata API. A joining host's user data
	// holds its join token, so neither containers nor the host's own
	// processes (Traefik in the host's network) may reach it.
	metadataService = netip.MustParseAddr("169.254.169.254")
)

// containerBridges are the interfaces that containers send from: clowder's
// stack bridges, Docker's default bridge, and the Compose networks that
// clowder did not create.
const containerBridges = `{ "cbr-*", "docker0", "br-*" }`

// markMask holds the bits of a connection's mark that clowder uses. clowder
// leaves the other bits unchanged. The packet mark of a packet from a
// container bridge is different: clowder sets the whole mark to these bits,
// because nft cannot merge two marks. Nothing else on the host marks such
// packets.
const markMask = 0xfff

// Ruleset is what goes into the nft table inet clowder.
type Ruleset struct {
	FloatingIP netip.Addr
	// ClusterHosts are the underlay and public addresses of every host,
	// sorted.
	ClusterHosts []netip.Addr
	// PrivateNetwork is the Hetzner private network between the hosts. It
	// is the zero value until the first configuration names it.
	PrivateNetwork netip.Prefix
	// Peers are the other hosts this one has a WireGuard interface to,
	// sorted.
	Peers []state.HostID
	// NodeAddress is this host's node address. A container's connection to
	// a forwarded port leaves from this address (hairpin).
	NodeAddress netip.Addr
	// Forwards are the published ports whose stacks run on other hosts,
	// sorted (SortForwards).
	Forwards []Forward
}

// Forward is a published port whose stack runs on another host. Connections
// to the port on the Floating IP go to that host's node address. Docker
// publishes the port there too.
type Forward struct {
	Protocol string // tcp or udp
	Port     uint16
	Target   netip.Addr
}

// SortForwards sorts forwards by protocol and port. Render writes them in
// this order.
func SortForwards(forwards []Forward) {
	slices.SortFunc(forwards, func(a, b Forward) int {
		return cmp.Or(strings.Compare(a.Protocol, b.Protocol), cmp.Compare(a.Port, b.Port))
	})
}

// forwardMark marks a connection that clowder sent to another host
// (Forwards). Docker's FORWARD chain lets only these connections pass
// (dockerRule).
const forwardMark = 0x200

// Render writes the table inet clowder. It deletes and recreates the table
// in one transaction, so nft -f replaces it atomically and leaves other
// rules (Docker's) alone. docs/developing/host.md (Network) describes each
// chain.
func Render(ruleset Ruleset) string {
	lines := []string{
		"table inet clowder",
		"delete table inet clowder",
		"table inet clowder {",
		"  set cluster_hosts {",
		"    type ipv4_addr",
	}
	if len(ruleset.ClusterHosts) > 0 {
		lines = append(lines, "    elements = { "+joinAddresses(ruleset.ClusterHosts)+" }")
	}
	lines = append(lines,
		"  }",
		"  chain marks {",
		"    type filter hook prerouting priority mangle; policy accept;",
	)
	// answers to a connection that came in from a peer go back to that peer
	// (peerTable). The connection keeps the peer's mark, and the container's
	// answer takes the mark from the connection. nft binds & before |, so
	// the mark's other bits stay and clowder's bits become the peer's.
	for _, peer := range ruleset.Peers {
		lines = append(lines, fmt.Sprintf(`    ct state new iifname "%s" ct mark set ct mark & %#x | %#x`,
			PeerInterface(peer), ^uint32(markMask), peerMark(peer)))
	}
	lines = append(lines,
		fmt.Sprintf("    iifname %s meta mark set ct mark & %#x", containerBridges, markMask),
		"  }",
		"  chain guard_in {",
		"    type filter hook input priority filter; policy accept;",
		"    iifname "+containerBridges+" fib saddr . iif oif missing counter drop",
		"    iifname "+containerBridges+" ct state established,related accept",
		fmt.Sprintf("    iifname %s ip daddr %s meta l4proto { tcp, udp } th dport 53 accept", containerBridges, Resolver),
	)
	if ruleset.FloatingIP.IsValid() {
		lines = append(lines,
			fmt.Sprintf("    iifname %s ip daddr %s tcp dport { 80, 443 } accept", containerBridges, ruleset.FloatingIP))
	}
	lines = append(lines,
		"    iifname "+containerBridges+" drop",
		"  }",
		"  chain guard_fwd {",
		"    type filter hook forward priority filter - 10; policy accept;",
		"    iifname "+containerBridges+" fib saddr . iif oif missing counter drop",
		"    iifname "+containerBridges+" ct state established,related accept",
		"    iifname "+containerBridges+" ct status dnat accept",
		fmt.Sprintf("    iifname %s ip daddr %s counter drop", containerBridges, metadataService),
		"    iifname "+containerBridges+" ip daddr @cluster_hosts counter drop",
	)
	if ruleset.PrivateNetwork.IsValid() {
		lines = append(lines,
			fmt.Sprintf("    iifname %s ip daddr %s counter drop", containerBridges, ruleset.PrivateNetwork))
	}
	lines = append(lines,
		fmt.Sprintf("    iifname %s ip daddr %s counter drop", containerBridges, Overlay),
		"  }",
		"  chain guard_out {",
		"    type filter hook output priority filter; policy accept;",
		fmt.Sprintf("    ip daddr %s counter drop", metadataService),
		"  }",
	)
	lines = append(lines, forwardChains(ruleset)...)
	lines = append(lines,
		// the WireGuard interfaces have a smaller MTU than the containers.
		// TCP through them learns the MTU in the handshake; UDP packets must
		// fit on their own.
		"  chain mss {",
		"    type filter hook forward priority mangle; policy accept;",
		fmt.Sprintf(`    oifname "%s*" tcp flags syn / syn,rst tcp option maxseg size set rt mtu`, peerInterfacePrefix),
		"  }",
		"}",
	)
	return strings.Join(lines, "\n") + "\n"
}

// forwardChains renders the DNAT of the forwards, which runs before Docker's
// (dstnat - 10), and the hairpin SNAT: an answer to a container's bridge
// address would reach a bridge of the same network on the stack's host.
// Every host has them, not only the one with the Floating IP, because its
// own containers need them too.
func forwardChains(ruleset Ruleset) []string {
	if !ruleset.FloatingIP.IsValid() || !ruleset.NodeAddress.IsValid() {
		return nil
	}
	lines := []string{
		"  chain forwards {",
		"    type nat hook prerouting priority dstnat - 10; policy accept;",
	}
	for _, forward := range ruleset.Forwards {
		lines = append(lines, fmt.Sprintf("    ip daddr %s %s dport %d ct mark set ct mark & %#x | %#x dnat ip to %s",
			ruleset.FloatingIP, forward.Protocol, forward.Port, ^uint32(markMask), forwardMark,
			netip.AddrPortFrom(forward.Target, forward.Port)))
	}
	return append(lines,
		"  }",
		"  chain hairpin {",
		"    type nat hook postrouting priority srcnat - 10; policy accept;",
		fmt.Sprintf("    iifname %s ct mark & %#x == %#x snat ip to %s",
			containerBridges, markMask, forwardMark, ruleset.NodeAddress),
		"  }",
	)
}

func joinAddresses(addresses []netip.Addr) string {
	listed := make([]string, len(addresses))
	for i, address := range addresses {
		listed[i] = address.String()
	}
	return strings.Join(listed, ", ")
}

// toolTimeout bounds nft, ip, wg and conntrack, which answer in
// milliseconds when the host is healthy.
const toolTimeout = 30 * time.Second
