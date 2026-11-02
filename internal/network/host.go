package network

import (
	"context"
	"io"
	"log/slog"
	"net"
	"strconv"
	"time"
)

// Runner runs a host tool with input on its standard input:
// command.RunWithInput, or a fake in tests.
type Runner func(ctx context.Context, timeout time.Duration, input, name string, args ...string) (string, error)

// Host is the host's real network, as the agent changes it.
type Host struct {
	// ResolvConf names the host's resolvers (/etc/resolv.conf).
	ResolvConf string
	// WireGuardKey is the file with the host's WireGuard private key.
	WireGuardKey string
	// Run runs ip, wg, nft and conntrack.
	Run    Runner
	Logger *slog.Logger
}

// ApplyRules renders the table inet clowder, checks it with nft and then
// loads it.
func (h Host) ApplyRules(ctx context.Context, ruleset Ruleset) error {
	rendered := Render(ruleset)
	for _, args := range [][]string{{"-c", "-f", "-"}, {"-f", "-"}} {
		if _, err := h.Run(ctx, toolTimeout, rendered, "nft", args...); err != nil {
			return err
		}
	}
	return nil
}

// StartResolver starts the containers' DNS resolver on Resolver:53.
func (h Host) StartResolver() (io.Closer, error) {
	upstreams, err := Upstreams(h.ResolvConf)
	if err != nil {
		return nil, err
	}
	return StartResolver(net.JoinHostPort(Resolver.String(), strconv.Itoa(53)), upstreams, h.Logger)
}
