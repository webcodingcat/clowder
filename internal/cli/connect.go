package cli

import (
	"crypto/tls"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/webcodingcat/clowder/internal/api"
	"github.com/webcodingcat/clowder/internal/buildinfo"
	"github.com/webcodingcat/clowder/internal/clusterdir"
	"github.com/webcodingcat/clowder/internal/pki"
)

// globals are the flags every command shares.
type globals struct {
	cluster string
	user    string
}

func (g *globals) register(root *cobra.Command) {
	root.PersistentFlags().StringVar(&g.cluster, "cluster", "",
		"cluster directory (default: $"+clusterdir.EnvVar+" or the working directory)")
	root.PersistentFlags().StringVar(&g.user, "user", "",
		"user whose credentials to use (default: $"+clusterdir.UserEnvVar+" or the only one)")
}

// connect reads the cluster directory and the user's credentials and returns
// a client for the cluster's agents, with the directory it read.
func (g *globals) connect() (*api.Client, *clusterdir.Directory, error) {
	path, err := clusterdir.Find(g.cluster)
	if err != nil {
		return nil, nil, err
	}
	directory, err := clusterdir.Load(path)
	if err != nil {
		return nil, nil, err
	}
	user, err := directory.User(g.user)
	if err != nil {
		return nil, nil, err
	}
	credentials, err := directory.Credentials(user)
	if err != nil {
		return nil, nil, err
	}
	certificate, err := tls.X509KeyPair([]byte(credentials.Certificate), []byte(credentials.Key))
	if err != nil {
		return nil, nil, fmt.Errorf("the credentials of %s: %w", user, err)
	}
	ca, err := pki.ParseCertificate([]byte(credentials.CA))
	if err != nil {
		return nil, nil, fmt.Errorf("the credentials of %s: %w", user, err)
	}
	return api.NewClient(directory.Config.Seeds, certificate, ca, buildinfo.Version), directory, nil
}
