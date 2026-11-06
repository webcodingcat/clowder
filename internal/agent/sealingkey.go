package agent

import (
	"context"
	"fmt"
	"io"

	"filippo.io/age"
	"github.com/spf13/cobra"

	"github.com/webcodingcat/clowder/internal/api"
	"github.com/webcodingcat/clowder/internal/buildinfo"
)

func newSealingKey() *cobra.Command {
	sealingKey := &cobra.Command{Use: "sealing-key", Short: "The cluster's sealing key, for SealedSecrets"}
	var public bool
	export := &cobra.Command{
		Use:   "export [--public]",
		Short: "Print the cluster's sealing key, to keep it offline",
		Long: "Asks this host's agent for the cluster's sealing key and prints it (AGE-SECRET-KEY-1...).\n" +
			"Every value of a SealedSecret is sealed for it. Export it right after init and keep it\n" +
			"offline, in a password manager: if the cluster is lost, it opens the values in your files\n" +
			"(clowder secret decrypt) and moves them to a new cluster (clowder secret rekey). Whoever\n" +
			"holds it can read every SealedSecret. With --public, it prints only the public half\n" +
			"(age1...), which clowder secret seal --cert takes to seal without a cluster. For example\n" +
			"  ssh root@<host> clowderd sealing-key export > sealing-key\n" +
			"  ssh root@<host> clowderd sealing-key export --public > sealing-key.pub",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), credentialsTimeout)
			defer cancel()
			return printSealingKey(ctx, cmd.OutOrStdout(), Files{Dir: DefaultDir}, public)
		},
	}
	export.Flags().BoolVar(&public, "public", false, "print only the public half, for clowder secret seal --cert")
	sealingKey.AddCommand(export)
	return sealingKey
}

// printSealingKey asks the host's own agent for the sealing key, just as
// printCredentials asks it for a certificate. The agent holds the data key
// that decrypts the sealing key in the state.
func printSealingKey(ctx context.Context, out io.Writer, files Files, public bool) error {
	host, err := files.readHost()
	if err != nil {
		return err
	}
	if err := host.checkControlPlane("sealing-key"); err != nil {
		return err
	}
	hostCredentials, err := files.readCredentials()
	if err != nil {
		return err
	}
	client := api.NewClient(host.apiAddresses(), api.DefaultConnectTimeout, hostCredentials.certificate,
		hostCredentials.ca, buildinfo.Version)
	var answer api.SealingKeyAnswer
	if err := client.Get(ctx, "/v1/internal/sealing-key", &answer); err != nil {
		return fmt.Errorf("get the sealing key from this host's agent: %w", err)
	}
	if !public {
		_, err = fmt.Fprintln(out, answer.Identity)
		return err
	}
	identity, err := age.ParseX25519Identity(answer.Identity)
	if err != nil {
		return fmt.Errorf("read the sealing key: %w", err)
	}
	_, err = fmt.Fprintln(out, identity.Recipient().String())
	return err
}
