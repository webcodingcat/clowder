package agent

import (
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/webcodingcat/clowder/internal/datakey"
	"github.com/webcodingcat/clowder/internal/pki"
	"github.com/webcodingcat/clowder/internal/recoverycode"
)

func newUser() *cobra.Command {
	user := &cobra.Command{
		Use:   "user",
		Short: "Help the cluster's users from a host",
	}
	user.AddCommand(&cobra.Command{
		Use:   "recovery-code <user>",
		Short: "Print the codes that give a user access back (changes nothing)",
		Long: "Prints a short code for clowder user recover (new credentials) and one for clowder recovery\n" +
			"rotate (a new recovery key), valid for 15 to 30 minutes and each once. It reads only the\n" +
			"data key, so it works while the agent is down, and it does not know the users: a mistyped\n" +
			"name gives codes that fit nowhere.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return printRecoveryCode(cmd.OutOrStdout(), Files{Dir: DefaultDir}, args[0], time.Now())
		},
	})
	return user
}

// printRecoveryCode prints the user's codes of now, one for each use, in
// the commands that take them. Root on a host holds
// every key of the cluster anyway, as with kubeadm kubeconfig user.
func printRecoveryCode(out io.Writer, files Files, user string, now time.Time) error {
	if err := pki.CheckUserName(user); err != nil {
		return err
	}
	dataKey, err := datakey.Load(files.dataKey())
	if err != nil {
		return err
	}
	key, err := dataKey.Derive(recoverycode.Purpose)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "For %s, valid for 15 to 30 minutes, each code once:\n\n"+
		"  clowder user recover %s --code %s\n"+
		"  clowder recovery rotate --code %s\n",
		user, user, recoverycode.Make(key, recoverycode.ForRecover, user, now),
		recoverycode.Make(key, recoverycode.ForRotate, user, now))
	return err
}
