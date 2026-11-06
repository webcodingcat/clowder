// Package network sets up the host's network for clowder: clowder0 with the
// node address, the WireGuard mesh to the other hosts with its routes, the
// nft table inet clowder and the containers' DNS resolver.
package network

import (
	"context"
	"fmt"
	"net/netip"

	"github.com/webcodingcat/clowder/internal/command"
	"github.com/webcodingcat/clowder/internal/state"
)

// Interface is the dummy interface that carries the node address. Raft, the
// API and published ports bind to that address, so it must exist even on a
// host without peers (one host, or all peers down).
const Interface = "clowder0"

// NodeAddress is the node address of a host: 10.77.0.<ID + 1>, in the
// overlay 10.77.0.0/24 that WireGuard connects. state.MaxHosts hosts fit
// in it.
func NodeAddress(id state.HostID) netip.Addr {
	return netip.AddrFrom4([4]byte{10, 77, 0, byte(id + 1)})
}

// SetUpInterface creates clowder0 if it is missing, brings it up and gives
// it the node address and the resolver's address. It does only what is
// missing. The interface does not survive a reboot, so the agent runs
// SetUpInterface at every start.
func (h Host) SetUpInterface(ctx context.Context, nodeAddress netip.Addr) error {
	// ip exits with 1 when the interface does not exist
	_, err := h.Run(ctx, toolTimeout, "", "ip", "link", "show", Interface)
	if code, exited := command.ExitCode(err); err != nil && (!exited || code != 1) {
		return fmt.Errorf("look for %s: %w", Interface, err)
	}
	if err != nil {
		if _, err := h.Run(ctx, toolTimeout, "", "ip", "link", "add", Interface, "type", "dummy"); err != nil {
			return err
		}
	}
	if _, err := h.Run(ctx, toolTimeout, "", "ip", "link", "set", Interface, "up"); err != nil {
		return err
	}
	for _, address := range []netip.Addr{nodeAddress, Resolver} {
		if _, err := h.Run(ctx, toolTimeout, "", "ip", "address", "replace", address.String()+"/32",
			"dev", Interface); err != nil {
			return err
		}
	}
	return nil
}
