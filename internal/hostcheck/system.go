package hostcheck

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
)

// System is the real host.
type System struct{}

// ReadFile reads a file of the host.
func (System) ReadFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

// Run runs a command and returns its standard output.
func (System) Run(ctx context.Context, name string, args ...string) (string, error) {
	output, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return string(output), fmt.Errorf("%s: %w", name, err)
	}
	return string(output), nil
}

// Addresses returns the IPv4 addresses on the host's interfaces.
func (System) Addresses() ([]netip.Addr, error) {
	interfaceAddresses, err := net.InterfaceAddrs()
	if err != nil {
		return nil, fmt.Errorf("list the addresses: %w", err)
	}
	var addresses []netip.Addr
	for _, interfaceAddress := range interfaceAddresses {
		if prefix, err := netip.ParsePrefix(interfaceAddress.String()); err == nil && prefix.Addr().Is4() {
			addresses = append(addresses, prefix.Addr())
		}
	}
	return addresses, nil
}
