// Package hoster is the cloud the hosts run in: for the leader it routes
// the Floating IP to the ingress host, tells which server a host runs on,
// and powers servers off (fencing) and on; clowderd recover marks the
// servers it takes out of the cluster, and an agent asks for the mark when
// it starts. Each uses only its part. Servers are named by their ServerRef,
// the hoster's own ID of the server.
package hoster

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"

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
	// ServerAddresses are the server's IPv4 addresses: private and public.
	ServerAddresses(ctx context.Context, serverRef string) ([]netip.Addr, error)
	// PowerOff cuts the server's power and returns once it is off: fencing
	// relies on it, so it must not return earlier.
	PowerOff(ctx context.Context, serverRef string) error
	// PowerOn switches the server on.
	PowerOn(ctx context.Context, serverRef string) error
	// SetRetired marks the server as taken out of its cluster by clowderd
	// recover (a label), with the serial of the retired host's certificate;
	// RetiredSerial reads it, "" for none. The agent of a retired host starts
	// nothing: with another retired one it would still have the old
	// cluster's majority. A server set up anew has another certificate.
	SetRetired(ctx context.Context, serverRef, serial string) error
	RetiredSerial(ctx context.Context, serverRef string) (string, error)
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

// ErrOtherServer is CheckServerRef's answer for a server that is not the
// host's.
var ErrOtherServer = errors.New("it is another server")

// CheckServerRef makes sure the server is the host's: each of the host's
// addresses (its underlay, private or, without a private network, public;
// and its public address if it has one) is one of the server's. A host
// names its server itself, and fencing powers off the server a ServerRef
// names; the hoster's token may power off any server of the project.
func CheckServerRef(ctx context.Context, client Hoster, serverRef string, hostAddresses ...netip.Addr) error {
	addresses, err := client.ServerAddresses(ctx, serverRef)
	if errors.Is(err, hetzner.ErrNoSuchServer) {
		// a server the project does not have is no server of the host
		return fmt.Errorf("check server %s: %w: %w", serverRef, err, ErrOtherServer)
	}
	if err != nil {
		return fmt.Errorf("check server %s: %w", serverRef, err)
	}
	for _, address := range hostAddresses {
		if address.IsValid() && !slices.Contains(addresses, address) {
			return fmt.Errorf("server %s has the addresses %v, not the host's %s: %w",
				serverRef, addresses, address, ErrOtherServer)
		}
	}
	return nil
}
