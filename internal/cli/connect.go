package cli

import (
	"crypto/tls"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/webcodingcat/clowder/internal/api"
	"github.com/webcodingcat/clowder/internal/buildinfo"
	"github.com/webcodingcat/clowder/internal/clusterdir"
	"github.com/webcodingcat/clowder/internal/pki"
)

// globals are the flags every command shares.
type globals struct {
	cluster        string
	connectTimeout time.Duration
}

func (g *globals) register(root *cobra.Command) {
	root.PersistentFlags().StringVar(&g.cluster, "cluster", "",
		"cluster directory (default: $"+clusterdir.EnvVar+" or the working directory)")
	root.PersistentFlags().DurationVar(&g.connectTimeout, "connect-timeout", api.DefaultConnectTimeout,
		"how long to wait for a host to take the connection before the next is tried")
	// checked before any command runs: every command that asks a host uses it
	root.PersistentPreRunE = func(*cobra.Command, []string) error {
		if g.connectTimeout <= 0 {
			return usage("--connect-timeout %s: give a positive duration like 5s", g.connectTimeout)
		}
		return nil
	}
}

// connect reads the cluster directory and the user's credentials for the
// cluster it names, and returns a client for the cluster's agents, with the
// directory it read.
func (g *globals) connect() (*api.Client, *clusterdir.Directory, error) {
	path, err := clusterdir.Find(g.cluster)
	if err != nil {
		return nil, nil, err
	}
	directory, err := clusterdir.Load(path)
	if err != nil {
		return nil, nil, err
	}
	credentials, err := clusterdir.ReadCredentials(directory.Config.Name)
	if err != nil {
		return nil, nil, err
	}
	certificate, err := tls.X509KeyPair([]byte(credentials.Certificate), []byte(credentials.Key))
	if err != nil {
		return nil, nil, fmt.Errorf("the credentials for cluster %s: %w", directory.Config.Name, err)
	}
	ca, err := pki.ParseCertificate([]byte(credentials.CA))
	if err != nil {
		return nil, nil, fmt.Errorf("the credentials for cluster %s: %w", directory.Config.Name, err)
	}
	return api.NewClient(directory.Config.Endpoints, g.connectTimeout, certificate, ca, buildinfo.Version), directory, nil
}
