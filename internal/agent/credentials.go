package agent

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
	"github.com/webcodingcat/clowder/internal/pki"
)

// credentialsTimeout bounds clowderd credentials' call to its own agent: a
// cutoff waits for the leader to commit it.
const credentialsTimeout = time.Minute

// CredentialsOptions are the flags of clowderd credentials.
type CredentialsOptions struct {
	// Name goes into the CLI's certificate, for the audit.
	Name string
	// Endpoints take the place of the hosts' public addresses, such as one
	// address in front of them.
	Endpoints   []string
	RevokeOlder bool
}

func newCredentials() *cobra.Command {
	var options CredentialsOptions
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
			return printCredentials(ctx, cmd.OutOrStdout(), Files{Dir: DefaultDir}, options)
		},
	}
	// no default here: the agent names a certificate without one
	// (pki.DefaultCLIName), so the rule has one place
	command.Flags().StringVar(&options.Name, "name", "",
		"the name in the certificate, which the agents' logs and the audit show as cli/<name>; it grants nothing "+
			"(default "+pki.DefaultCLIName+")")
	command.Flags().StringArrayVar(&options.Endpoints, "endpoint", nil,
		"an address the CLI asks, instead of the hosts' public addresses (repeat it for more)")
	command.Flags().BoolVar(&options.RevokeOlder, "revoke-older", false,
		"refuse every CLI certificate issued before the new one, on every host")
	return command
}

// printCredentials makes the CLI's key here and has the host's own agent
// issue its certificate. Root on a host holds every key of the cluster
// anyway, as with kubeadm's admin.conf; the agent only spares it the CA's
// key, which is sealed in the state.
func printCredentials(ctx context.Context, out io.Writer, files Files, options CredentialsOptions) error {
	host, err := files.readHost()
	if err != nil {
		return err
	}
	hostCredentials, err := files.readCredentials()
	if err != nil {
		return err
	}
	key, err := pki.NewKey()
	if err != nil {
		return err
	}
	client := api.NewClient(host.apiAddresses(), api.DefaultConnectTimeout, hostCredentials.certificate,
		hostCredentials.ca, buildinfo.Version)
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
