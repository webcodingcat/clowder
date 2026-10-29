// Package network sets up the host's networking for clowder. So far it
// holds only the node address; WireGuard, routing and nftables follow.
package network

import (
	"context"
	"fmt"
	"net/netip"
	"os/exec"
	"strings"
)

// Interface is the dummy interface that carries the node address, so the
// address is there even on a host without peers (one host, or all peers
// down): Raft, the API and published ports bind to it.
const Interface = "clowder0"

// EnsureNodeAddress creates clowder0 if it is missing, brings it up and
// gives it the node address. It does only what is missing, so the agent runs
// it at every start (the interface does not survive a reboot).
func EnsureNodeAddress(ctx context.Context, address netip.Addr) error {
	if err := ip(ctx, "link", "show", Interface); err != nil {
		if err := ip(ctx, "link", "add", Interface, "type", "dummy"); err != nil {
			return err
		}
	}
	if err := ip(ctx, "link", "set", Interface, "up"); err != nil {
		return err
	}
	return ip(ctx, "address", "replace", address.String()+"/32", "dev", Interface)
}

// ip runs the ip command of iproute2.
func ip(ctx context.Context, args ...string) error {
	output, err := exec.CommandContext(ctx, "ip", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ip %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return nil
}
