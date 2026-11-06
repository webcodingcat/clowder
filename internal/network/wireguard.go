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

// The WireGuard mesh has one interface per peer rather than one for all
// peers. Each peer then has its own port, route and routing table. So an
// answer without SNAT, which carries a client's address, goes back through
// the peer that its connection came from.
const (
	peerInterfacePrefix = "clw-"
	// wireGuardBasePort plus a host's ID is the port of the interface to
	// that host (stackspec reserves the ports for state.MaxHosts hosts)
	wireGuardBasePort = 51800
	// peerTableBase plus a peer's ID is the peer's routing table, and the
	// priority of the rule that leads there
	peerTableBase = 100
	// peerMarkBase plus a peer's ID marks the connections that came from
	// the peer. For state.MaxHosts hosts, this goes up to 0x11f, within
	// markMask.
	peerMarkBase = 0x100
	// wireGuardOverhead is the most that WireGuard adds to a packet, over
	// an IPv6 underlay (over IPv4 it is 60). An interface's MTU is the
	// underlay's MTU minus this, as wg-quick sets it.
	wireGuardOverhead = 80
)

// Mesh is this host's part of the WireGuard mesh: an interface to each peer.
type Mesh struct {
	Self state.HostID `json:"self"`
	// Peers are the other hosts, sorted by ID.
	Peers []Peer `json:"peers"`
}

// Peer is another host, as WireGuard reaches it.
type Peer struct {
	ID        state.HostID `json:"id"`
	PublicKey string       `json:"public_key"`
	Underlay  netip.Addr   `json:"underlay"`
}

// PeerInterface is the WireGuard interface to the peer with the ID.
func PeerInterface(id state.HostID) string {
	return peerInterfacePrefix + strconv.Itoa(int(id))
}

func peerTable(id state.HostID) int {
	return peerTableBase + int(id)
}

func peerMark(id state.HostID) uint32 {
	return peerMarkBase + uint32(id)
}

// peerRule is the routing rule that sends a peer's answers to its table, in
// the form that ip rule takes as a selector and ip -j rule show prints.
type peerRule struct {
	Priority int    `json:"priority"`
	Mark     string `json:"fwmark"`
	Mask     string `json:"fwmask"`
	Table    string `json:"table"`
}

func wantedRule(id state.HostID) peerRule {
	return peerRule{
		Priority: peerTable(id),
		Mark:     fmt.Sprintf("%#x", peerMark(id)),
		Mask:     fmt.Sprintf("%#x", markMask),
		Table:    strconv.Itoa(peerTable(id)),
	}
}

// selector is the rule as the arguments that ip rule takes after add or
// delete. ip -j rule show leaves out the mask when it is all ones. It
// leaves out the mark when there is none (a rule changed by hand).
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
// wg syncconf reads it. The configuration holds the private key, so it goes
// to wg on its standard input and never into a file or an argument.
//
// Each side listens on the port of the other side's ID: host 0's clw-2
// listens on 51802, and host 2's clw-0 on 51800.
//
// AllowedIPs allows every address. Answers without SNAT, and a DNAT to
// another host, carry a client's address. The peers are the cluster's own
// hosts, authenticated by their keys.
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

// ApplyMesh makes the WireGuard interfaces, routes and routing rules match
// the mesh. A peer that fails does not hold up the others, and ApplyMesh
// returns all errors together.
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
		// check the key here: wg quotes a line it cannot read in its error,
		// which would put the key into the log
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

// connectPeer takes the peer's existing rules and deletes all of them
// except one copy of the wanted rule.
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
		// the reverse path filter, even in loose mode, drops what arrives on
		// an interface without an IPv4 address
		{"address", "replace", node + "/32", "dev", name, "noprefixroute"},
		// what the host sends to the peer comes from the node address,
		// because the peer's Traefik trusts it and the peer's services bind
		// to it
		{"route", "replace", peerNode + "/32", "dev", name, "src", node},
		{"route", "replace", "default", "dev", name, "table", rule.Table},
	}
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

// disconnectPeer first forgets the connections that came from the peer,
// because the peer's ID may come back with another host. The routes go away
// with the interface. If the agent stops halfway, the interface or rule that
// is left makes the next pass call disconnectPeer again.
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

// peerRules reads the peers' rules from ip -j rule show. A rule belongs to a
// peer when its priority and its table are the peer's (peerTable), whatever
// its mark.
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

// unmarshalList reads the JSON list that ip -j prints. For an empty list,
// ip prints nothing.
func unmarshalList(listed string, list any) error {
	if strings.TrimSpace(listed) == "" {
		return nil
	}
	return json.Unmarshal([]byte(listed), list)
}
