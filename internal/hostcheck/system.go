package hostcheck

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"time"

	"github.com/webcodingcat/clowder/internal/command"
)

// System is the real host.
type System struct{}

// ReadFile reads a file of the host.
func (System) ReadFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

// commandTimeout bounds each command. lvs or drbdadm can hang in the kernel
// while DRBD suspends I/O, and a check must never hang with them.
const commandTimeout = 10 * time.Second

// Run runs a command and returns its standard output; one that hangs is
// left behind after commandTimeout.
func (System) Run(ctx context.Context, name string, args ...string) (string, error) {
	return command.Run(ctx, commandTimeout, name, args...)
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
