package cli

import (
	"bufio"
	"context"
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
)

// bootstrapFlags are the flags of clowder cluster bootstrap.
type bootstrapFlags struct {
	name, seed, keyFile, recoveryOut string
	force                            bool
}

func newBootstrap(g *globals) *cobra.Command {
	var flags bootstrapFlags
	command := &cobra.Command{
		Use:   "bootstrap <user>",
		Short: "Become the user of a new cluster and set up its directory",
		Long: "Asks the first host for the cluster's CA and trusts it only if the host proves it knows\n" +
			"the bootstrap key. Makes the recovery key, shows it once and asks for its last 8\n" +
			"characters (or writes it to --recovery-out), then gets the user's first certificate.\n" +
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
			// before the cluster is asked: once it has its user, a
			// bootstrap cannot be repeated
			if err := clusterdir.CheckBootstrap(path, flags.name, flags.force); err != nil {
				return err
			}
			bootstrapKey, err := pki.ReadBootstrapKey(flags.keyFile)
			if err != nil {
				return err
			}
			// before a recovery key is made: nobody may hold two and not know
			// which one counts
			cluster, err := api.FindCluster(cmd.Context(), flags.seed, []byte(bootstrapKey), buildinfo.Version)
			if errors.Is(err, api.ErrBootstrapped) {
				return fmt.Errorf("%w. If that was an earlier try of yours, the recovery key it gave out is the one "+
					"that counts; %s", err, accessByCode(user, flags.name, flags.seed))
			}
			if err != nil {
				return err
			}
			recovery, err := age.GenerateX25519Identity()
			if err != nil {
				return err
			}
			if err := handOverRecoveryKey(cmd, recovery, flags.recoveryOut); err != nil {
				return err
			}
			credentials, err := becomeFirstUser(cmd.Context(), cluster, user, recovery)
			if err != nil {
				return err
			}
			// the credentials first: without them the cluster's user
			// is lost, while cluster.yaml can be written by hand
			if err := clusterdir.WriteCredentials(flags.name, credentials); err != nil {
				return fmt.Errorf("%w. The cluster has %s as its user and the recovery key just made counts; "+
					"%s, and write cluster.yaml after the README's example", err, user,
					accessByCode(user, flags.name, flags.seed))
			}
			newDirectory := clusterdir.NewDirectory{Path: path, Name: flags.name, Seed: flags.seed}
			if err := createDirectory(cmd.Context(), newDirectory, credentials, cluster, recovery); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s is the user of cluster %s. Next:\n"+
				"  1. fill in cluster.yaml: the hoster with its token (clowder secret encrypt --cluster-yaml),\n"+
				"     the Floating IP, the storage classes\n"+
				"  2. clowder cluster apply\n", user, flags.name)
			return err
		},
	}
	command.Flags().StringVar(&flags.name, "name", "", "the cluster's name, by which the CLI finds your credentials")
	command.Flags().BoolVar(&flags.force, "force", false, "replace your credentials for a cluster of the same name")
	command.Flags().StringVar(&flags.seed, "seed", "", "address of the first host")
	command.Flags().StringVar(&flags.keyFile, "key-file", "",
		"a file with the cluster's bootstrap key (from OpenTofu); not the key itself, which shell histories would keep")
	command.Flags().StringVar(&flags.recoveryOut, "recovery-out", "",
		"write the recovery key to this new file instead of showing it (scripts, no terminal)")
	for _, flag := range []string{"name", "seed", "key-file"} {
		_ = command.MarkFlagRequired(flag) // the flags exist; this cannot fail
	}
	return command
}

// becomeFirstUser makes the user's key and gets its certificate from the
// cluster, which takes recovery's public key as the cluster's recovery key.
func becomeFirstUser(ctx context.Context, cluster *api.TrustedCluster, user string,
	recovery *age.X25519Identity,
) (clusterdir.Credentials, error) {
	key, err := pki.NewKey()
	if err != nil {
		return clusterdir.Credentials{}, err
	}
	certificatePEM, err := cluster.Bootstrap(ctx, user, key.Public().(ed25519.PublicKey), recovery.Recipient().String())
	if err != nil {
		return clusterdir.Credentials{}, err
	}
	keyPEM, err := pki.EncodeKey(key)
	if err != nil {
		return clusterdir.Credentials{}, err
	}
	return clusterdir.Credentials{User: user, CA: cluster.CAPEM, Certificate: certificatePEM, Key: string(keyPEM)}, nil
}

// createDirectory writes the cluster directory with cluster.yaml to fill
// in, once the cluster, asked with the new credentials, has the recovery
// key just made.
func createDirectory(ctx context.Context, directory clusterdir.NewDirectory, credentials clusterdir.Credentials,
	cluster *api.TrustedCluster, recovery *age.X25519Identity,
) error {
	certificate, err := tls.X509KeyPair([]byte(credentials.Certificate), []byte(credentials.Key))
	if err != nil {
		return err
	}
	client := api.NewClient([]string{directory.Seed}, certificate, cluster.CA, buildinfo.Version)
	_, encryption, err := clusterRecipients(ctx, client)
	if err != nil {
		return err
	}
	// a guard against a fault of clowder's own: a second bootstrap stops
	// before, or the cluster refuses it
	if encryption.Recovery != recovery.Recipient().String() {
		return fmt.Errorf("the cluster's recovery key is %s, not the one just made; "+
			"make a new one with clowder recovery rotate and a code from a host", encryption.Recovery)
	}
	return clusterdir.Create(directory)
}

// accessByCode says how a user of the cluster gets in without credentials.
func accessByCode(user, cluster, seed string) string {
	return fmt.Sprintf("get access with a code from a host: clowderd user recovery-code %s, as root there, "+
		"then clowder user recover %s --code <code> --name %s --seed %s", user, user, cluster, seed)
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
