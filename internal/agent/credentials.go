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
	"github.com/webcodingcat/clowder/internal/clusterdir"
	"github.com/webcodingcat/clowder/internal/pki"
)

// credentialsTimeout bounds clowderd credentials' call to its own agent: a
// cutoff waits for the leader to commit it.
const credentialsTimeout = time.Minute

func newCredentials() *cobra.Command {
	var revokeOlder bool
	command := &cobra.Command{
		Use:   "credentials",
		Short: "Print new credentials for the CLI (as kubeadm kubeconfig user)",
		Long: "Asks this host's agent for a new CLI certificate and prints credentials.yaml: the CLI's\n" +
			"key and certificate and the cluster's CA. Put it where the CLI looks for it\n" +
			"(clowder/clusters/<cluster>/credentials.yaml in your config directory). Whoever holds it\n" +
			"controls the cluster. With --revoke-older, every CLI certificate issued before this one\n" +
			"stops working, such as one on a lost laptop; that needs the cluster's leader.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), credentialsTimeout)
			defer cancel()
			return printCredentials(ctx, cmd.OutOrStdout(), Files{Dir: DefaultDir}, revokeOlder)
		},
	}
	command.Flags().BoolVar(&revokeOlder, "revoke-older", false,
		"refuse every CLI certificate issued before the new one, on every host")
	return command
}

// printCredentials makes the CLI's key here and has the host's own agent
// issue its certificate. Root on a host holds every key of the cluster
// anyway, as with kubeadm's admin.conf; the agent only spares it the CA's
// key, which is sealed in the state.
func printCredentials(ctx context.Context, out io.Writer, files Files, revokeOlder bool) error {
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
	request := api.CredentialsRequest{PublicKey: key.Public().(ed25519.PublicKey), RevokeOlder: revokeOlder}
	var answer api.CredentialsAnswer
	if err := client.Act(ctx, "/v1/internal/credentials", request, &answer); err != nil {
		return fmt.Errorf("get a CLI certificate from this host's agent: %w", err)
	}
	keyPEM, err := pki.EncodeKey(key)
	if err != nil {
		return err
	}
	content, err := yaml.Marshal(clusterdir.Credentials{
		CA: answer.CACertificate, Certificate: answer.Certificate, Key: string(keyPEM),
	})
	if err != nil {
		return err
	}
	_, err = out.Write(content)
	return err
}
