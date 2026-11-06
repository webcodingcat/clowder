// Package hostsetup holds what init and join share to set a host up: their
// flags, the new host that the flags describe, and the start of the agent.
package hostsetup

import (
	"context"
	"fmt"
	"net/netip"
	"os"

	"github.com/spf13/cobra"

	"github.com/webcodingcat/clowder/internal/clowderd/service"
	"github.com/webcodingcat/clowder/internal/hostrole"
)

// HostFlags are the flags that init and join share.
type HostFlags struct {
	underlay, publicAddress, roles string
	serverID                       int64
	start                          bool
}

// NewHost is the host that init or join sets up, as its flags and its
// hostname describe it.
type NewHost struct {
	Name                    string
	Roles                   hostrole.Set
	Underlay, PublicAddress netip.Addr
	ServerID                int64
}

// Add adds the flags to cmd; roles is the default of --roles.
func (f *HostFlags) Add(cmd *cobra.Command, roles hostrole.Set) {
	cmd.Flags().StringVar(&f.roles, "roles", roles.String(), "the host's roles: "+hostrole.Rule)
	cmd.Flags().StringVar(&f.underlay, "underlay", "", "the server's address in the Hetzner private network")
	cmd.Flags().StringVar(&f.publicAddress, "public-address", "", "the server's public IPv4, where users reach the API")
	cmd.Flags().Int64Var(&f.serverID, "server-id", 0, "the Hetzner Cloud ID of this server")
	cmd.Flags().BoolVar(&f.start, "start", true, "enable and start "+service.Unit+" afterwards")
	for _, name := range []string{"underlay", "public-address", "server-id"} {
		_ = cmd.MarkFlagRequired(name) // the flags exist; this cannot fail
	}
}

// Parse reads the flags, and the hostname as the host's name in the
// cluster.
func (f *HostFlags) Parse() (NewHost, error) {
	host := NewHost{ServerID: f.serverID}
	var err error
	if host.Name, err = os.Hostname(); err != nil {
		return host, fmt.Errorf("read the hostname, the host's name in the cluster: %w", err)
	}
	if host.Roles, err = hostrole.Parse(f.roles); err != nil {
		return host, fmt.Errorf("--roles: %w", err)
	}
	if host.Underlay, err = netip.ParseAddr(f.underlay); err != nil {
		return host, fmt.Errorf("--underlay: %w", err)
	}
	if host.PublicAddress, err = netip.ParseAddr(f.publicAddress); err != nil {
		return host, fmt.Errorf("--public-address: %w", err)
	}
	return host, nil
}

// StartService enables and starts the agent, unless --start=false.
func (f *HostFlags) StartService(ctx context.Context) error {
	if !f.start {
		return nil
	}
	return service.EnableAndStart(ctx)
}
