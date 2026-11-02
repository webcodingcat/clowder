package network

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strconv"
	"strings"

	"github.com/webcodingcat/clowder/internal/command"
	"github.com/webcodingcat/clowder/internal/state"
)

// The WireGuard mesh has an interface per peer rather than one for all: each
// peer then has a port, a route and a routing table of its own, so an answer
// without SNAT (it carries a client's address) goes back through the peer
// its connection came from.
const (
	peerInterfacePrefix = "clw-"
	// wireGuardBasePort plus a host's ID is the port of the interface to it
	// (stackspec reserves the ports of state.MaxHosts)
	wireGuardBasePort = 51800
	// peerTableBase plus a peer's ID is its routing table and the priority of
	// the rule that leads there
	peerTableBase = 100
	// peerMarkBase plus a peer's ID marks the connections that came from it:
	// up to 0x11f for state.MaxHosts, within markMask
	peerMarkBase = 0x100
	// wireGuardOverhead is what WireGuard adds to a packet at most (over an
	// IPv6 underlay; over IPv4 it is 60): an interface's MTU is the
	// underlay's less this, as wg-quick sets it
	wireGuardOverhead = 80
)

// Mesh is this host's part of the WireGuard mesh: an interface to each peer.
type Mesh struct {
	Self state.HostID
	// Peers are the other hosts, sorted by ID.
	Peers []Peer
}

// Peer is another host, as WireGuard reaches it.
type Peer struct {
	ID        state.HostID
	PublicKey string
	Underlay  netip.Addr
}

// PeerInterface is the WireGuard interface to the peer with the ID.
func PeerInterface(id state.HostID) string {
	return peerInterfacePrefix + strconv.Itoa(int(id))
}

// peerTable is the routing table of the answers to the peer with the ID.
func peerTable(id state.HostID) int {
	return peerTableBase + int(id)
}

// peerMark marks the connections that came from the peer with the ID.
func peerMark(id state.HostID) uint32 {
	return peerMarkBase + uint32(id)
}

// peerRule is the routing rule that sends a peer's answers to its table, as
// ip rule takes its selector and ip -j rule show prints it.
type peerRule struct {
	Priority int    `json:"priority"`
	Mark     string `json:"fwmark"`
	Mask     string `json:"fwmask"`
	Table    string `json:"table"`
}

// wantedRule is the rule for the peer with the ID.
func wantedRule(id state.HostID) peerRule {
	return peerRule{
		Priority: peerTable(id),
		Mark:     fmt.Sprintf("%#x", peerMark(id)),
		Mask:     fmt.Sprintf("%#x", markMask),
		Table:    strconv.Itoa(peerTable(id)),
	}
}

// selector is the rule in ip rule's words, after add or delete. ip -j rule
// show leaves the mask out when it is all ones, and the mark when there is
// none (a rule changed by hand).
func (r peerRule) selector() []string {
	selector := []string{"priority", strconv.Itoa(r.Priority)}
	switch {
	case r.Mark != "" && r.Mask != "":
		selector = append(selector, "fwmark", r.Mark+"/"+r.Mask)
	case r.Mark != "":
		selector = append(selector, "fwmark", r.Mark)
	}
	return append(selector, "lookup", r.Table)
}

// RenderWireGuard writes the configuration of the interface to a peer, as
// wg syncconf reads it. It holds the private key, so it goes to wg on its
// standard input and never into a file or an argument.
//
// Each side listens on the port of the other's ID, so the interface to peer
// m listens on wireGuardBasePort + m and reaches m at wireGuardBasePort +
// self: host 0's clw-2 listens on 51802, host 2's clw-0 on 51800.
// AllowedIPs is everything: answers without SNAT and DNAT to another host
// carry a client's address, and the peers are the cluster's own hosts,
// authenticated by their keys.
func RenderWireGuard(privateKey string, self state.HostID, peer Peer) string {
	return fmt.Sprintf(`[Interface]
PrivateKey = %s
ListenPort = %d

[Peer]
PublicKey = %s
Endpoint = %s
AllowedIPs = 0.0.0.0/0
`, privateKey, wireGuardBasePort+int(peer.ID), peer.PublicKey,
		netip.AddrPortFrom(peer.Underlay, uint16(wireGuardBasePort+int(self))))
}

// ApplyMesh brings the WireGuard interfaces to the mesh: for each peer its
// interface, its route and the routing table of its answers; for a former
// peer it removes all three and forgets the connections that came from it.
// At every pass it sets each peer's interface and routes anew (wg syncconf
// and ip replace change only what differs) and replaces a rule that is not
// as wanted. A peer that fails holds up no other; the errors come together.
func (h Host) ApplyMesh(ctx context.Context, mesh Mesh) error {
	listed, err := h.Run(ctx, toolTimeout, "", "ip", "-j", "link", "show", "type", "wireguard")
	if err != nil {
		return err
	}
	interfaces, err := peerInterfaces(listed)
	if err != nil {
		return err
	}
	listed, err = h.Run(ctx, toolTimeout, "", "ip", "-j", "rule", "show")
	if err != nil {
		return err
	}
	rules, err := peerRules(listed)
	if err != nil {
		return err
	}

	var failures []error
	wanted := map[state.HostID]bool{}
	if len(mesh.Peers) > 0 {
		read, err := os.ReadFile(h.WireGuardKey)
		if err != nil {
			return fmt.Errorf("read the WireGuard key: %w", err)
		}
		// wg names a line it cannot read in its error, which would put the
		// key into the log
		key := strings.TrimSpace(string(read))
		if decoded, err := base64.StdEncoding.DecodeString(key); err != nil || len(decoded) != 32 {
			return fmt.Errorf("%s holds no WireGuard key", h.WireGuardKey)
		}
		for _, peer := range mesh.Peers {
			wanted[peer.ID] = true
			if err := h.connectPeer(ctx, key, mesh.Self, peer,
				interfaces[peer.ID], rules[peer.ID]); err != nil {
				failures = append(failures, fmt.Errorf("connect to host %d: %w", peer.ID, err))
			}
		}
	}
	for id := range state.HostID(state.MaxHosts) {
		_, hasRules := rules[id]
		if (interfaces[id] || hasRules) && !wanted[id] {
			if err := h.disconnectPeer(ctx, id, interfaces[id], rules[id]); err != nil {
				failures = append(failures, fmt.Errorf("disconnect from host %d: %w", id, err))
			}
		}
	}
	return errors.Join(failures...)
}

// connectPeer sets up the interface to a peer, its route and the routing
// table of its answers. hasInterface says whether the interface exists;
// rules are the peer's rules there are, of which any but the wanted one
// goes.
func (h Host) connectPeer(ctx context.Context, privateKey string, self state.HostID, peer Peer,
	hasInterface bool, rules []peerRule,
) error {
	name := PeerInterface(peer.ID)
	node, peerNode := NodeAddress(self).String(), NodeAddress(peer.ID).String()
	rule := wantedRule(peer.ID)
	mtu, err := h.underlayMTU(ctx, peer.Underlay)
	if err != nil {
		return err
	}
	if !hasInterface {
		if _, err := h.Run(ctx, toolTimeout, "", "ip", "link", "add", name, "type", "wireguard"); err != nil {
			return err
		}
	}
	if _, err := h.Run(ctx, toolTimeout, RenderWireGuard(privateKey, self, peer),
		"wg", "syncconf", name, "/dev/stdin"); err != nil {
		return err
	}
	steps := [][]string{
		{"link", "set", name, "mtu", strconv.Itoa(mtu - wireGuardOverhead), "up"},
		// the reverse path filter drops, even when loose, what arrives on an
		// interface without an IPv4 address (the spike of phase 0 found it;
		// docs/roadmap.md)
		{"address", "replace", node + "/32", "dev", name, "noprefixroute"},
		// what the host sends to the peer carries the node address, the one
		// the peer's Traefik trusts and its services bind to
		{"route", "replace", peerNode + "/32", "dev", name, "src", node},
		{"route", "replace", "default", "dev", name, "table", rule.Table},
	}
	// keep one copy of the wanted rule; any other of the peer's goes
	found := false
	for _, existing := range rules {
		if existing == rule && !found {
			found = true
			continue
		}
		steps = append(steps, append([]string{"rule", "delete"}, existing.selector()...))
	}
	if !found {
		steps = append(steps, append([]string{"rule", "add"}, rule.selector()...))
	}
	for _, args := range steps {
		if _, err := h.Run(ctx, toolTimeout, "", "ip", args...); err != nil {
			return err
		}
	}
	return nil
}

// disconnectPeer removes what connectPeer set up for a former peer (the
// routes go with the interface). First it forgets the connections that came
// from it, as the peer's ID may come back with another host: should the
// agent stop halfway, the interface or the rule left brings it back here.
func (h Host) disconnectPeer(ctx context.Context, id state.HostID, hasInterface bool, rules []peerRule) error {
	// conntrack exits with 1 when no connection had the mark
	_, err := h.Run(ctx, toolTimeout, "", "conntrack", "--delete", "--mark",
		fmt.Sprintf("%#x/%#x", peerMark(id), markMask))
	if code, exited := command.ExitCode(err); err != nil && (!exited || code != 1) {
		return err
	}
	if hasInterface {
		if _, err := h.Run(ctx, toolTimeout, "", "ip", "link", "delete", PeerInterface(id)); err != nil {
			return err
		}
	}
	for _, rule := range rules {
		args := append([]string{"rule", "delete"}, rule.selector()...)
		if _, err := h.Run(ctx, toolTimeout, "", "ip", args...); err != nil {
			return err
		}
	}
	return nil
}

// underlayMTU is the MTU of the interface the host reaches an underlay
// address through.
func (h Host) underlayMTU(ctx context.Context, address netip.Addr) (int, error) {
	output, err := h.Run(ctx, toolTimeout, "", "ip", "-j", "route", "get", address.String())
	if err != nil {
		return 0, err
	}
	var routes []struct {
		Dev string `json:"dev"`
	}
	if err := json.Unmarshal([]byte(output), &routes); err != nil || len(routes) != 1 || routes[0].Dev == "" {
		return 0, fmt.Errorf("read the route to %s from %q", address, output)
	}
	output, err = h.Run(ctx, toolTimeout, "", "ip", "-j", "link", "show", "dev", routes[0].Dev)
	if err != nil {
		return 0, err
	}
	var links []struct {
		MTU int `json:"mtu"`
	}
	if err := json.Unmarshal([]byte(output), &links); err != nil || len(links) != 1 || links[0].MTU <= wireGuardOverhead {
		return 0, fmt.Errorf("read the MTU of %s from %q", routes[0].Dev, output)
	}
	return links[0].MTU, nil
}

// peerInterfaces reads which peers have an interface from ip -j link show
// type wireguard. Only clw-<host ID> is clowder's.
func peerInterfaces(listed string) (map[state.HostID]bool, error) {
	var links []struct {
		Name string `json:"ifname"`
	}
	if err := unmarshalList(listed, &links); err != nil {
		return nil, fmt.Errorf("read the WireGuard interfaces: %w", err)
	}
	peers := map[state.HostID]bool{}
	for _, link := range links {
		number, found := strings.CutPrefix(link.Name, peerInterfacePrefix)
		id, err := strconv.Atoi(number)
		if !found || err != nil || id < 0 || id >= state.MaxHosts {
			continue
		}
		// clw-02 is not the name clowder gives host 2
		if PeerInterface(state.HostID(id)) == link.Name {
			peers[state.HostID(id)] = true
		}
	}
	return peers, nil
}

// peerRules reads the peers' rules from ip -j rule show: a rule is a peer's
// when its priority and its table are that peer's (peerTable), whatever its
// mark.
func peerRules(listed string) (map[state.HostID][]peerRule, error) {
	var rules []peerRule
	if err := unmarshalList(listed, &rules); err != nil {
		return nil, fmt.Errorf("read the routing rules: %w", err)
	}
	peers := map[state.HostID][]peerRule{}
	for _, rule := range rules {
		if rule.Priority < peerTableBase || rule.Priority >= peerTableBase+state.MaxHosts {
			continue
		}
		id := state.HostID(rule.Priority - peerTableBase)
		if rule.Table == strconv.Itoa(peerTable(id)) {
			peers[id] = append(peers[id], rule)
		}
	}
	return peers, nil
}

// unmarshalList reads the JSON list ip -j prints; it prints nothing for an
// empty one.
func unmarshalList(listed string, list any) error {
	if strings.TrimSpace(listed) == "" {
		return nil
	}
	return json.Unmarshal([]byte(listed), list)
}
