package network

import (
	"context"
	"fmt"
	"net/netip"
	"strconv"

	"github.com/webcodingcat/clowder/internal/command"
)

// dockerRule lets the connections that clowder sent to another host pass
// Docker's FORWARD chain. The chain's policy drops everything that Docker
// did not publish itself. conntrack's DNAT state and the mark apply in both
// directions, so the rule lets the answers pass too.
var dockerRule = []string{
	"-m", "conntrack", "--ctstate", "DNAT",
	"-m", "connmark", "--mark", fmt.Sprintf("%#x/%#x", forwardMark, markMask), "-j", "ACCEPT",
}

// AllowForwards puts dockerRule first in Docker's chain DOCKER-USER,
// unless it is already there. Docker creates the chain when it starts and
// keeps the rules that others add to it.
func (h Host) AllowForwards(ctx context.Context) error {
	check := append([]string{"-w", "5", "-C", "DOCKER-USER"}, dockerRule...)
	_, err := h.Run(ctx, toolTimeout, "", "iptables", check...)
	// iptables -C exits with 1 when the rule is missing
	code, exited := command.ExitCode(err)
	missing := err != nil && exited && code == 1
	switch {
	case err == nil:
		return nil
	case !missing:
		return fmt.Errorf("look for the rule of the forwarded ports in DOCKER-USER: %w", err)
	}
	insert := append([]string{"-w", "5", "-I", "DOCKER-USER", "1"}, dockerRule...)
	if _, err := h.Run(ctx, toolTimeout, "", "iptables", insert...); err != nil {
		return fmt.Errorf("put the rule of the forwarded ports into DOCKER-USER: %w", err)
	}
	return nil
}

// ForgetForwards forgets the connections to the Floating IP on the
// forwards' ports. conntrack keeps a connection's target, so a UDP flow
// that never pauses would still go to the old host after its stack moved.
// Once the connection is forgotten, its next packet passes the current rules.
func (h Host) ForgetForwards(ctx context.Context, floatingIP netip.Addr, forwards []Forward) error {
	for _, forward := range forwards {
		// conntrack exits with 1 when no connection matched
		_, err := h.Run(ctx, toolTimeout, "", "conntrack", "--delete", "--proto", forward.Protocol,
			"--orig-dst", floatingIP.String(), "--orig-port-dst", strconv.Itoa(int(forward.Port)))
		if code, exited := command.ExitCode(err); err != nil && (!exited || code != 1) {
			return fmt.Errorf("forget the connections to %s/%d: %w", forward.Protocol, forward.Port, err)
		}
	}
	return nil
}
