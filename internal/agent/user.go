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
		Short: "Print a code that gives a user access back (changes nothing)",
		Long: "Prints a short code for clowder user recover (new credentials) and clowder recovery rotate\n" +
			"(a new recovery key), valid for 15 to 30 minutes. It reads only the data key, so it works\n" +
			"while the agent is down, and it does not know the users: a mistyped name gives a code\n" +
			"that fits nowhere.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return printRecoveryCode(cmd.OutOrStdout(), Files{Dir: DefaultDir}, args[0], time.Now())
		},
	})
	return user
}

// printRecoveryCode prints the user's code of now. Root on a host holds
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
	_, err = fmt.Fprintf(out, "%s\n\nfor %s, valid for 15 to 30 minutes: clowder user recover %s --code <code>,\n"+
		"or clowder recovery rotate --code <code>\n", recoverycode.Make(key, user, now), user, user)
	return err
}
