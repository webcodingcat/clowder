package cli

import (
	"bufio"
	"crypto/ed25519"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"filippo.io/age"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/webcodingcat/clowder/internal/api"
	"github.com/webcodingcat/clowder/internal/buildinfo"
	"github.com/webcodingcat/clowder/internal/clusterdir"
	"github.com/webcodingcat/clowder/internal/pki"
	"github.com/webcodingcat/clowder/internal/secretvalue"
)

func newBootstrap(g *globals) *cobra.Command {
	var name, seed, keyFile, recoveryOut string
	var force bool
	command := &cobra.Command{
		Use:   "bootstrap <user>",
		Short: "Become the first user of a new cluster and set up its directory",
		Long: "Asks the first host for the cluster's CA and trusts it only if the host proves it knows\n" +
			"the bootstrap key. Makes the recovery key, shows it once and asks for its last 8\n" +
			"characters (or writes it to --recovery-out), then gets the first user's certificate.\n" +
			"Writes your credentials into your config directory, under the cluster's name, and\n" +
			"cluster.yaml to fill in into the cluster directory.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			user := args[0]
			if err := pki.CheckUserName(user); err != nil {
				return err
			}
			path, err := clusterdir.Target(g.cluster)
			if err != nil {
				return err
			}
			// before the cluster is asked: once it has its first user, a
			// bootstrap cannot be repeated
			if err := clusterdir.CheckBootstrap(path, name, force); err != nil {
				return err
			}
			bootstrapKey, err := pki.ReadBootstrapKey(keyFile)
			if err != nil {
				return err
			}
			cluster, err := api.FindCluster(cmd.Context(), seed, []byte(bootstrapKey), buildinfo.Version)
			if err != nil {
				return err
			}
			// before a recovery key is made: nobody may hold two and not know
			// which one counts
			if cluster.Bootstrapped {
				return errors.New("the cluster has its first user already. If that was an earlier try of " +
					"yours, the recovery key it gave out is the one that counts; access comes back with a code " +
					"from a host (clowderd user recovery-code, clowder user recover)")
			}
			recovery, err := age.GenerateX25519Identity()
			if err != nil {
				return err
			}
			if err := handOverRecoveryKey(cmd, recovery, recoveryOut); err != nil {
				return err
			}
			key, err := pki.NewKey()
			if err != nil {
				return err
			}
			certificatePEM, err := cluster.Bootstrap(cmd.Context(), user, key.Public().(ed25519.PublicKey),
				recovery.Recipient().String())
			if err != nil {
				return err
			}
			keyPEM, err := pki.EncodeKey(key)
			if err != nil {
				return err
			}
			// the credentials first: without them the cluster's first user
			// is lost, while cluster.yaml can be written by hand
			credentials := clusterdir.Credentials{User: user, CA: cluster.CAPEM, Certificate: certificatePEM, Key: string(keyPEM)}
			if err := clusterdir.WriteCredentials(name, credentials); err != nil {
				return err
			}
			certificate, err := tls.X509KeyPair([]byte(certificatePEM), keyPEM)
			if err != nil {
				return err
			}
			client := api.NewClient([]string{seed}, certificate, cluster.CA, buildinfo.Version)
			recipients, encryption, err := clusterRecipients(cmd.Context(), client)
			if err != nil {
				return err
			}
			// a guard against a fault of clowder's own: a second bootstrap
			// stops above, or the cluster refuses it
			if encryption.Recovery != recovery.Recipient().String() {
				return fmt.Errorf("the cluster's recovery key is %s, not the one just made; "+
					"make a new one with clowder recovery rotate and a code from a host", encryption.Recovery)
			}
			joinKey, err := secretvalue.Encrypt(secretvalue.Value{For: secretvalue.Cluster, Bytes: []byte(bootstrapKey)},
				recipients...)
			if err != nil {
				return err
			}
			if err := clusterdir.Create(clusterdir.NewDirectory{Path: path, Name: name, Seed: seed, JoinKey: joinKey}); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s is the first user of cluster %s. Next:\n"+
				"  1. fill in cluster.yaml: the hoster with its token (clowder secret encrypt --cluster-yaml),\n"+
				"     the Floating IP, the storage classes\n"+
				"  2. clowder cluster apply\n", user, name)
			return err
		},
	}
	command.Flags().StringVar(&name, "name", "", "the cluster's name, by which the CLI finds your credentials")
	command.Flags().BoolVar(&force, "force", false, "replace your credentials for a cluster of the same name")
	command.Flags().StringVar(&seed, "seed", "", "address of the first host")
	command.Flags().StringVar(&keyFile, "key-file", "",
		"a file with the cluster's bootstrap key (from OpenTofu); not the key itself, which shell histories would keep")
	command.Flags().StringVar(&recoveryOut, "recovery-out", "",
		"write the recovery key to this new file instead of showing it (scripts, no terminal)")
	for _, flag := range []string{"name", "seed", "key-file"} {
		_ = command.MarkFlagRequired(flag) // the flags exist; this cannot fail
	}
	return command
}

// confirmChars is how much of the recovery key the user types back: enough
// to show it was written down, not so much that typing it is a chore.
const confirmChars = 8

// handOverRecoveryKey gives the recovery key to the user, the only time it
// exists outside the user's keeping: into a new file, or shown on the
// terminal with its last characters typed back. Nothing has reached the
// cluster yet, so a key that is not taken costs nothing.
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

// confirmKey asks for the key's last characters until they come right, at
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
