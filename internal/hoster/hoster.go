// Package hoster is the cloud the hosts run in, as the leader uses it: it
// routes the Floating IP to the ingress host and, for fencing (phase 2),
// powers servers off and on. Servers are named by their ServerRef, the
// hoster's own ID of the server.
package hoster

import (
	"context"
	"fmt"
	"net/netip"

	"github.com/webcodingcat/clowder/internal/clusterspec"
	"github.com/webcodingcat/clowder/internal/hoster/hetzner"
)

// Hoster is what clowder asks of a hoster's API.
type Hoster interface {
	// FloatingIPTarget is the server the Floating IP is routed to, "" for
	// none.
	FloatingIPTarget(ctx context.Context, ip netip.Addr) (serverRef string, err error)
	// AssignFloatingIP routes the Floating IP to the server and returns once
	// the hoster has done it.
	AssignFloatingIP(ctx context.Context, ip netip.Addr, serverRef string) error
	// PowerOff cuts the server's power and returns once it is off.
	PowerOff(ctx context.Context, serverRef string) error
	// PowerOn starts the server and returns once it runs.
	PowerOn(ctx context.Context, serverRef string) error
}

// New is the plugin cluster.yaml names, with the token in clear.
func New(config clusterspec.Hoster, token string) (Hoster, error) {
	switch config.Type {
	case "hetzner":
		return hetzner.New(config.API, token), nil
	default:
		return nil, fmt.Errorf("hoster %q is not supported", config.Type)
	}
}
