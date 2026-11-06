// Package credentials is clowderd credentials: it prints new credentials
// for the CLI.
package credentials

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"

	"github.com/webcodingcat/clowder/internal/api"
	"github.com/webcodingcat/clowder/internal/buildinfo"
	"github.com/webcodingcat/clowder/internal/clientconfig"
	"github.com/webcodingcat/clowder/internal/clowderd/hostfiles"
	"github.com/webcodingcat/clowder/internal/pki"
)

// credentialsTimeout limits the call from clowderd credentials to the agent
// on the same host. With --revoke-older, the call also waits until the
// leader commits the cutoff.
const credentialsTimeout = time.Minute

// Options are the flags of clowderd credentials.
type Options struct {
	// Name goes into the CLI's certificate, for the audit.
	Name string
	// Endpoints replace the hosts' public addresses, for example with one
	// address in front of the hosts.
	Endpoints   []string
	RevokeOlder bool
}

// NewCommand returns clowderd credentials, which reads the host's files.
func NewCommand(files hostfiles.Files) *cobra.Command {
	var options Options
	command := &cobra.Command{
		Use:   "credentials",
		Short: "Print new credentials for the CLI (as kubeadm kubeconfig user)",
		Long: "Asks this host's agent for a new CLI certificate and prints a CLI config with one context,\n" +
			"named after the cluster: the endpoints, the cluster's CA, the CLI's key and certificate.\n" +
			"Add it to yours with clowder config import, for example\n" +
			"  ssh root@<host> clowderd credentials | clowder config import -\n" +
			"Whoever holds it controls the cluster. The certificate lasts a year; fetch a new one before\n" +
			"it ends. The endpoints are the public addresses of the cluster's hosts now, unless\n" +
			"--endpoint names others. With --revoke-older, every CLI certificate issued before this one\n" +
			"stops working, such as one on a lost laptop; that needs the cluster's leader.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			for _, endpoint := range options.Endpoints {
				if !clientconfig.ValidEndpoint(endpoint) {
					return fmt.Errorf("--endpoint %q is neither an address nor a host name", endpoint)
				}
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), credentialsTimeout)
			defer cancel()
			return Print(ctx, cmd.OutOrStdout(), files, options)
		},
	}
	// no default here: the agent names a certificate that has no name
	// (pki.DefaultCLIName), so the rule lives in one place only
	command.Flags().StringVar(&options.Name, "name", "",
		"the name in the certificate, which the agents' logs and the audit show as cli/<name>; it grants nothing "+
			"(default "+pki.DefaultCLIName+")")
	command.Flags().StringArrayVar(&options.Endpoints, "endpoint", nil,
		"an address the CLI asks, instead of the hosts' public addresses (repeat it for more)")
	command.Flags().BoolVar(&options.RevokeOlder, "revoke-older", false,
		"refuse every CLI certificate issued before the new one, on every host")
	return command
}

// Print creates the CLI's key on this host and asks the host's own agent to
// issue the certificate. Root on a host can read every key of the cluster
// anyway, as with kubeadm's admin.conf. The agent only spares root from
// handling the CA's key, which the state keeps encrypted.
func Print(ctx context.Context, out io.Writer, files hostfiles.Files, options Options) error {
	host, err := files.ReadHost()
	if err != nil {
		return err
	}
	if err := host.CheckControlPlane("credentials"); err != nil {
		return err
	}
	hostCredentials, err := files.ReadCredentials(host.Roles)
	if err != nil {
		return err
	}
	key, err := pki.NewKey()
	if err != nil {
		return err
	}
	client := api.NewClient(host.APIAddresses(), api.DefaultConnectTimeout, hostCredentials.Certificate,
		hostCredentials.CA, buildinfo.Version)
	request := api.CredentialsRequest{
		PublicKey: key.Public().(ed25519.PublicKey), Name: options.Name, RevokeOlder: options.RevokeOlder,
	}
	var answer api.CredentialsAnswer
	if err := client.Act(ctx, "/v1/internal/credentials", request, &answer); err != nil {
		return fmt.Errorf("get a CLI certificate from this host's agent: %w", err)
	}
	keyPEM, err := pki.EncodeKey(key)
	if err != nil {
		return err
	}
	endpoints := answer.Endpoints
	if len(options.Endpoints) > 0 {
		endpoints = options.Endpoints
	}
	cliContext := clientconfig.Context{
		Name: answer.ClusterName, Endpoints: endpoints,
		CA: answer.CACertificate, Certificate: answer.Certificate, Key: string(keyPEM),
	}
	config := clientconfig.Config{CurrentContext: cliContext.Name, Contexts: []clientconfig.Context{cliContext}}
	content, err := yaml.Marshal(config)
	if err != nil {
		return err
	}
	_, err = out.Write(content)
	return err
}
