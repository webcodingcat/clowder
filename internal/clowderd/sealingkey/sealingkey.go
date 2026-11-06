// Package sealingkey is clowderd sealing-key: it prints the cluster's
// sealing key, to keep it offline.
package sealingkey

import (
	"context"
	"fmt"
	"io"
	"time"

	"filippo.io/age"
	"github.com/spf13/cobra"

	"github.com/webcodingcat/clowder/internal/api"
	"github.com/webcodingcat/clowder/internal/buildinfo"
	"github.com/webcodingcat/clowder/internal/clowderd/hostfiles"
)

// exportTimeout limits the call from clowderd sealing-key export to the
// agent on the same host.
const exportTimeout = time.Minute

// NewCommand returns clowderd sealing-key with its subcommand export, which
// reads the host's files.
func NewCommand(files hostfiles.Files) *cobra.Command {
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
			ctx, cancel := context.WithTimeout(cmd.Context(), exportTimeout)
			defer cancel()
			return Print(ctx, cmd.OutOrStdout(), files, public)
		},
	}
	export.Flags().BoolVar(&public, "public", false, "print only the public half, for clowder secret seal --cert")
	sealingKey.AddCommand(export)
	return sealingKey
}

// Print asks the host's own agent, which holds the data key that decrypts
// the sealing key in the state.
func Print(ctx context.Context, out io.Writer, files hostfiles.Files, public bool) error {
	host, err := files.ReadHost()
	if err != nil {
		return err
	}
	if err := host.CheckControlPlane("sealing-key"); err != nil {
		return err
	}
	hostCredentials, err := files.ReadCredentials(host.Roles)
	if err != nil {
		return err
	}
	client := api.NewClient(host.APIAddresses(), api.DefaultConnectTimeout, hostCredentials.Certificate,
		hostCredentials.CA, buildinfo.Version)
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
