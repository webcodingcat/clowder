package hostcheck

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"time"
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

// Run runs a command and returns its standard output. A command that does not
// finish in time is left behind: a process in state D ignores even SIGKILL,
// so waiting for it would hang the check.
func (System) Run(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	type result struct {
		output []byte
		err    error
	}
	done := make(chan result, 1)
	go func() {
		output, err := exec.CommandContext(ctx, name, args...).Output()
		done <- result{output, err}
	}()
	select {
	case finished := <-done:
		if finished.err != nil {
			return string(finished.output), fmt.Errorf("%s: %w", name, finished.err)
		}
		return string(finished.output), nil
	case <-ctx.Done():
		return "", fmt.Errorf("%s: no answer within %s: %w", name, commandTimeout, ctx.Err())
	}
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
