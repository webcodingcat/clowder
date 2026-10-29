package network

import (
	"context"
	"io"
	"log/slog"
	"net"
	"strconv"
)

// Host is the host's real network, as the agent changes it.
type Host struct {
	// ResolvConf names the host's resolvers (/etc/resolv.conf).
	ResolvConf string
	Logger     *slog.Logger
}

// ApplyRules renders and loads the table inet clowder.
func (Host) ApplyRules(ctx context.Context, ruleset Ruleset) error {
	return Apply(ctx, Render(ruleset))
}

// StartResolver starts the containers' DNS resolver on Resolver:53.
func (h Host) StartResolver() (io.Closer, error) {
	upstreams, err := Upstreams(h.ResolvConf)
	if err != nil {
		return nil, err
	}
	return StartResolver(net.JoinHostPort(Resolver.String(), strconv.Itoa(53)), upstreams, h.Logger)
}
