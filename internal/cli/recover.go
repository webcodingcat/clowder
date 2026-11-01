package cli

import (
	"cmp"
	"crypto/ed25519"
	"errors"
	"fmt"
	"os"

	"filippo.io/age"
	"github.com/spf13/cobra"

	"github.com/webcodingcat/clowder/internal/api"
	"github.com/webcodingcat/clowder/internal/buildinfo"
	"github.com/webcodingcat/clowder/internal/clusterdir"
	"github.com/webcodingcat/clowder/internal/pki"
	"github.com/webcodingcat/clowder/internal/recoverycode"
)

func newUser(g *globals) *cobra.Command {
	user := &cobra.Command{
		Use:   "user",
		Short: "Your access to the cluster",
	}
	user.AddCommand(newUserRecover(g))
	return user
}

func newUserRecover(g *globals) *cobra.Command {
	var codeText, name, seed string
	command := &cobra.Command{
		Use:   "recover <user>",
		Short: "Get new credentials with a code from a host; your other certificates stop working",
		Long: "Gets new credentials without the old ones, with a code from a host (clowderd user\n" +
			"recovery-code <user>, as root there). The cluster revokes every other certificate of the\n" +
			"user, so a stolen laptop is locked out. The cluster's name and seeds come from the cluster\n" +
			"directory; without one, give --name and --seed. Credentials under that name are replaced.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			user := args[0]
			if err := pki.CheckUserName(user); err != nil {
				return err
			}
			code, err := recoverycode.Parse(codeText)
			if err != nil {
				return err
			}
			name, seeds, err := recoveryTarget(g.cluster, name, seed)
			if err != nil {
				return err
			}
			// the key is made here and never leaves this machine
			key, err := pki.NewKey()
			if err != nil {
				return err
			}
			cluster, err := api.FindClusterByCode(cmd.Context(), seeds, user, code, buildinfo.Version)
			if err != nil {
				return err
			}
			certificatePEM, err := cluster.Recover(cmd.Context(), key.Public().(ed25519.PublicKey))
			if err != nil {
				return err
			}
			keyPEM, err := pki.EncodeKey(key)
			if err != nil {
				return err
			}
			replaced, err := hasCredentials(name)
			if err != nil {
				return err
			}
			credentials := clusterdir.Credentials{User: user, CA: cluster.CAPEM, Certificate: certificatePEM, Key: string(keyPEM)}
			if err := clusterdir.WriteCredentials(name, credentials); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if replaced {
				if _, err := fmt.Fprintf(out, "Replaced your credentials for cluster %s: their certificate "+
					"is revoked now anyway.\n", name); err != nil {
					return err
				}
			}
			_, err = fmt.Fprintf(out, "%s is back in cluster %s; every other certificate of %s is revoked.\n",
				user, name, user)
			return err
		},
	}
	command.Flags().StringVar(&codeText, "code", "", "the code from clowderd user recovery-code on a host")
	command.Flags().StringVar(&name, "name", "", "the cluster's name (default: from the cluster directory)")
	command.Flags().StringVar(&seed, "seed", "", "address of a host (default: the cluster directory's seeds)")
	_ = command.MarkFlagRequired("code") // the flag exists; this cannot fail
	return command
}

// recoveryTarget is the cluster a recovery goes to: its name and seeds
// from the flags, and what they leave out from the cluster directory. A
// recovery may well start on a machine without one.
func recoveryTarget(clusterFlag, name, seed string) (string, []string, error) {
	if name != "" && seed != "" {
		return name, []string{seed}, nil
	}
	path, err := clusterdir.Find(clusterFlag)
	if err != nil {
		return "", nil, usage("give --name and --seed, or run where the cluster directory is: %w", err)
	}
	directory, err := clusterdir.Load(path)
	if err != nil {
		return "", nil, err
	}
	seeds := directory.Config.Seeds
	if seed != "" {
		seeds = []string{seed}
	}
	return cmp.Or(name, directory.Config.Name), seeds, nil
}

// hasCredentials tells whether there are credentials for the cluster of
// that name.
func hasCredentials(cluster string) (bool, error) {
	path, err := clusterdir.CredentialsPath(cluster)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("look for credentials: %w", err)
	}
	return true, nil
}

func newRecovery(g *globals) *cobra.Command {
	recovery := &cobra.Command{
		Use:   "recovery",
		Short: "The cluster's recovery key",
	}
	recovery.AddCommand(newRecoveryRotate(g))
	return recovery
}

func newRecoveryRotate(g *globals) *cobra.Command {
	var codeText, recoveryOut string
	command := &cobra.Command{
		Use:   "rotate",
		Short: "Make a new recovery key, with your credentials and a code from a host",
		Long: "Makes a new recovery key, shows it once and asks for its last 8 characters (or writes it\n" +
			"to --recovery-out), then puts it in place of the old one. It needs your own code from a\n" +
			"host besides your credentials (clowderd user recovery-code <you>), so stolen credentials\n" +
			"cannot swap the key. Afterwards, encrypt your files for it: clowder secret rekey.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			code, err := recoverycode.Parse(codeText)
			if err != nil {
				return err
			}
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
			request := api.RotateRequest{Recipient: recovery.Recipient().String(), Code: code.String()}
			if _, err := client.Post(cmd.Context(), "/v1/recovery/rotate", request); err != nil {
				return fmt.Errorf("%w: the recovery key just made does not count, the old one still does", err)
			}
			_, err = fmt.Fprint(cmd.OutOrStdout(), "The new recovery key counts from now on. Encrypt your files "+
				"for it: clowder secret rekey -f <file|directory>,\nin every repository. Values in the Git history "+
				"stay readable with the old key: if it got into\nwrong hands, change the values themselves.\n")
			return err
		},
	}
	command.Flags().StringVar(&codeText, "code", "", "your code from clowderd user recovery-code on a host")
	command.Flags().StringVar(&recoveryOut, "recovery-out", "",
		"write the recovery key to this new file instead of showing it (scripts, no terminal)")
	_ = command.MarkFlagRequired("code") // the flag exists; this cannot fail
	return command
}
