// Package network sets up the host's networking for clowder. So far it
// holds only the node address; WireGuard, routing and nftables follow.
package network

import (
	"context"
	"fmt"
	"net/netip"
	"os/exec"
	"strings"

	"github.com/webcodingcat/clowder/internal/state"
)

// Interface is the dummy interface that carries the node address, so the
// address is there even on a host without peers (one host, or all peers
// down): Raft, the API and published ports bind to it.
const Interface = "clowder0"

// NodeAddress is the node address of a host: 10.77.0.<ID + 1>, in the
// overlay 10.77.0.0/24 that WireGuard connects (state.MaxHosts fits in it).
func NodeAddress(id state.HostID) netip.Addr {
	return netip.AddrFrom4([4]byte{10, 77, 0, byte(id + 1)})
}

// SetUpInterface creates clowder0 if it is missing, brings it up and gives
// it the node address and the resolver's address. It does only what is
// missing, so the agent runs it at every start (the interface does not
// survive a reboot).
func (Host) SetUpInterface(ctx context.Context, nodeAddress netip.Addr) error {
	if err := ip(ctx, "link", "show", Interface); err != nil {
		if err := ip(ctx, "link", "add", Interface, "type", "dummy"); err != nil {
			return err
		}
	}
	if err := ip(ctx, "link", "set", Interface, "up"); err != nil {
		return err
	}
	for _, address := range []netip.Addr{nodeAddress, Resolver} {
		if err := ip(ctx, "address", "replace", address.String()+"/32", "dev", Interface); err != nil {
			return err
		}
	}
	return nil
}

// ip runs the ip command of iproute2.
func ip(ctx context.Context, args ...string) error {
	output, err := exec.CommandContext(ctx, "ip", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ip %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return nil
}
