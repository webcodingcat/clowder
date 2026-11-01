package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"filippo.io/age"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/webcodingcat/clowder/internal/api"
	"github.com/webcodingcat/clowder/internal/secretvalue"
)

func newSecret(g *globals) *cobra.Command {
	secret := &cobra.Command{Use: "secret", Short: "Values encrypted for the cluster"}
	secret.AddCommand(newEncrypt(g))
	return secret
}

// blockIndent indents the armor of a printed block: deeper than any key of
// cluster.yaml up to three levels down; a deeper key needs it indented more.
const blockIndent = "    "

func newEncrypt(g *globals) *cobra.Command {
	var forClusterYAML bool
	command := &cobra.Command{
		Use:   "encrypt --cluster-yaml",
		Short: "Encrypt a value for cluster.yaml",
		Long: "Encrypts a value for the cluster and the recovery key and prints it tagged !secret, to\n" +
			"put after a key of cluster.yaml (indent the armor deeper than the key). Only the cluster\n" +
			"opens it. The value is asked for without echo on a terminal, else read from stdin, without\n" +
			"one line end at its end. The recipients come from the cluster each time.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, _, err := g.connect()
			if err != nil {
				return err
			}
			recipients, _, err := clusterRecipients(cmd.Context(), client)
			if err != nil {
				return err
			}
			value, err := readValue(cmd.InOrStdin(), cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			armored, err := secretvalue.Encrypt(secretvalue.Value{For: secretvalue.Cluster, Bytes: value}, recipients...)
			if err != nil {
				return err
			}
			_, err = fmt.Fprint(cmd.OutOrStdout(), secretvalue.Block(armored, blockIndent))
			return err
		},
	}
	// not --cluster: that names the cluster directory, for every command
	command.Flags().BoolVar(&forClusterYAML, "cluster-yaml", false, "a value for cluster.yaml")
	_ = command.MarkFlagRequired("cluster-yaml") // the flag exists; this cannot fail
	return command
}

// clusterRecipients asks the cluster whom every value is encrypted for: its
// own key and the recovery key.
func clusterRecipients(ctx context.Context, client *api.Client) ([]age.Recipient, api.Encryption, error) {
	var encryption api.Encryption
	if err := client.Get(ctx, "/v1/encryption", &encryption); err != nil {
		return nil, encryption, err
	}
	if encryption.Recovery == "" {
		return nil, encryption, errors.New("the cluster has no recovery key")
	}
	var recipients []age.Recipient
	for _, text := range []string{encryption.Cluster, encryption.Recovery} {
		recipient, err := age.ParseX25519Recipient(text)
		if err != nil {
			return nil, encryption, fmt.Errorf("the cluster's recipients: %w", err)
		}
		recipients = append(recipients, recipient)
	}
	return recipients, encryption, nil
}

// readValue reads the value to encrypt: asked for without echo on a
// terminal, else all of in without one line end at its end, which echo and
// most files add.
func readValue(in io.Reader, prompt io.Writer) ([]byte, error) {
	var value []byte
	if file, ok := in.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
		if _, err := fmt.Fprint(prompt, "value: "); err != nil {
			return nil, err
		}
		read, err := term.ReadPassword(int(file.Fd()))
		_, _ = fmt.Fprintln(prompt) // the line end the hidden input did not show
		if err != nil {
			return nil, fmt.Errorf("read the value: %w", err)
		}
		value = read
	} else {
		read, err := io.ReadAll(in)
		if err != nil {
			return nil, fmt.Errorf("read the value: %w", err)
		}
		text := string(read)
		if trimmed, ok := strings.CutSuffix(text, "\n"); ok {
			text = strings.TrimSuffix(trimmed, "\r")
		}
		value = []byte(text)
	}
	if len(value) == 0 {
		return nil, errors.New("the value is empty")
	}
	return value, nil
}
