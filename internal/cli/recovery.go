package cli

import (
	"fmt"

	"filippo.io/age"
	"github.com/spf13/cobra"

	"github.com/webcodingcat/clowder/internal/api"
)

func newRecovery(g *globals) *cobra.Command {
	recovery := &cobra.Command{
		Use:   "recovery",
		Short: "The cluster's recovery key",
	}
	recovery.AddCommand(newRecoveryRotate(g))
	return recovery
}

func newRecoveryRotate(g *globals) *cobra.Command {
	var recoveryOut string
	command := &cobra.Command{
		Use:   "rotate",
		Short: "Make a new recovery key",
		Long: "Makes a new recovery key, shows it once and asks for its last 8 characters (or writes it\n" +
			"to --recovery-out), then puts it in place of the old one. Afterwards, encrypt your files\n" +
			"for it: clowder secret rekey.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, _, err := g.connect()
			if err != nil {
				return err
			}
			recovery, err := age.GenerateX25519Identity()
			if err != nil {
				return err
			}
			if err := handOverRecoveryKey(cmd, recovery, recoveryOut); err != nil {
				return err
			}
			request := api.RotateRequest{Recipient: recovery.Recipient().String()}
			if _, err := client.Post(cmd.Context(), "/v1/recovery/rotate", request); err != nil {
				return fmt.Errorf("%w: the recovery key just made does not count, the old one still does", err)
			}
			_, err = fmt.Fprint(cmd.OutOrStdout(), "The new recovery key counts from now on. Encrypt your files "+
				"for it: clowder secret rekey -f <file|directory>,\nin every repository. Values in the Git history "+
				"stay readable with the old key: if it got into\nwrong hands, change the values themselves.\n")
			return err
		},
	}
	command.Flags().StringVar(&recoveryOut, "recovery-out", "",
		"write the recovery key to this new file instead of showing it (scripts, no terminal)")
	return command
}
