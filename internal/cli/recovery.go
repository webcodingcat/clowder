package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"filippo.io/age"
	"github.com/spf13/cobra"
	"golang.org/x/term"

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
		Short: "Make the cluster's recovery key, its first or a new one",
		Long: "Makes a recovery key, shows it once and asks for its last 8 characters (or writes it to\n" +
			"--recovery-out), then puts it in place. A new cluster has no recovery key, and nothing\n" +
			"can be encrypted for the cluster until this command has made the first one. A new key\n" +
			"replaces the old one; afterwards, encrypt your files for it with clowder secret rekey.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := g.connect()
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
				return fmt.Errorf("%w: the recovery key just made does not count; an older one still does", err)
			}
			_, err = fmt.Fprint(cmd.OutOrStdout(), "The recovery key counts from now on. If it replaced an older "+
				"one, encrypt your files for it:\nclowder secret rekey -f <file|directory>, in every repository. "+
				"Values in the Git history stay\nreadable with the old key: if it got into wrong hands, change the "+
				"values themselves.\n")
			return err
		},
	}
	command.Flags().StringVar(&recoveryOut, "recovery-out", "",
		"write the recovery key to this new file instead of showing it (scripts, no terminal)")
	return command
}

// confirmChars is how much of the recovery key the user types back: enough
// to show that it was written down, but not so much that typing it is a
// chore.
const confirmChars = 8

// handOverRecoveryKey gives the recovery key to the user. This is the only
// time the key exists outside the user's keeping. It goes into a new file,
// or it is shown on the terminal and the user types its last characters
// back. Nothing has reached the cluster yet, so a key that the user does
// not take costs nothing.
func handOverRecoveryKey(cmd *cobra.Command, recovery *age.X25519Identity, out string) error {
	if out != "" {
		file, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return fmt.Errorf("write the recovery key: %w", err)
		}
		if _, err := fmt.Fprintln(file, recovery.String()); err != nil {
			_ = file.Close() // the write error is the one to report
			return fmt.Errorf("write the recovery key: %w", err)
		}
		return file.Close()
	}
	in, ok := cmd.InOrStdin().(*os.File)
	if !ok || !term.IsTerminal(int(in.Fd())) {
		return errors.New("no terminal to show the recovery key on: write it to a file with --recovery-out")
	}
	key := recovery.String()
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "The recovery key of the cluster. Keep it in your password manager and "+
		"offline:\nwith it, the cluster's values can be read when the cluster is lost. It is shown only now.\n\n"+
		"    %s\n\n", key)
	if err != nil {
		return err
	}
	return confirmKey(in, cmd.OutOrStdout(), key)
}

// confirmKey asks for the key's last characters until they are right, at
// most three times.
func confirmKey(in io.Reader, out io.Writer, key string) error {
	want := key[len(key)-confirmChars:]
	lines := bufio.NewScanner(in)
	for range 3 {
		if _, err := fmt.Fprintf(out, "Its last %d characters: ", confirmChars); err != nil {
			return err
		}
		if !lines.Scan() {
			break
		}
		if strings.EqualFold(strings.TrimSpace(lines.Text()), want) {
			return nil
		}
	}
	return errors.New("the recovery key was not confirmed; nothing was sent to the cluster")
}
