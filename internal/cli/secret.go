package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"

	"filippo.io/age"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/webcodingcat/clowder/internal/api"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/resource"
	"github.com/webcodingcat/clowder/internal/secretvalue"
)

func newSecret(g *globals) *cobra.Command {
	secret := &cobra.Command{Use: "secret", Short: "Values encrypted for the cluster"}
	secret.AddCommand(newEncrypt(g), newRekey(g), newDecrypt())
	return secret
}

// blockIndent indents the armor of a printed value one level deeper than
// its key in spec.data.
const blockIndent = "      "

func newEncrypt(g *globals) *cobra.Command {
	var literals, fromFiles []string
	command := &cobra.Command{
		Use:   "encrypt [<namespace>/]<name> (--from-literal <key>=<value> | --from-file <key>=<path|->)...",
		Short: "Encrypt values for a Secret and print the Secret",
		Long: "Encrypts values for the cluster and the recovery key and prints the whole Secret to apply\n" +
			"(in default if no namespace is given), with each value under its key in spec.data. The\n" +
			"flags work like those of kubectl create secret generic. --from-file takes a file's\n" +
			"content, or with - one value from stdin: on a terminal it asks without echo, otherwise it\n" +
			"reads stdin without one line end at the end. --from-literal puts the value on the command\n" +
			"line, where the shell's history keeps it. The values are bound to the Secret, and no\n" +
			"other Secret accepts them. Only the cluster can decrypt them. The recipients come from the\n" +
			"cluster each time.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			namespace, name, err := secretName(args[0])
			if err != nil {
				return err
			}
			values, err := secretValues(literals, fromFiles, cmd.InOrStdin(), cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			client, err := g.connect()
			if err != nil {
				return err
			}
			recipients, err := clusterRecipients(cmd.Context(), client)
			if err != nil {
				return err
			}
			out := fmt.Sprintf("apiVersion: %s\nkind: %s\nmetadata: {namespace: %s, name: %s}\nspec:\n  data:\n",
				resource.APIVersion, resource.KindSecret, namespace, name)
			for _, key := range slices.Sorted(maps.Keys(values)) {
				armored, err := secretvalue.Encrypt(secretvalue.Value{For: names.ID(namespace, name), Bytes: values[key]},
					recipients...)
				if err != nil {
					return err
				}
				out += "    " + key + ": " + secretvalue.Block(armored, blockIndent)
			}
			_, err = fmt.Fprint(cmd.OutOrStdout(), out)
			return err
		},
	}
	command.Flags().StringArrayVar(&literals, "from-literal", nil, "a key and its value, <key>=<value>")
	command.Flags().StringArrayVar(&fromFiles, "from-file", nil,
		"a key and the file of its value, <key>=<path>, or <key>=- for stdin")
	return command
}

// secretValues reads the values of encrypt's flags by key. Each key may
// appear once, and stdin may be used at most once.
func secretValues(literals, fromFiles []string, stdin io.Reader, prompt io.Writer) (map[string][]byte, error) {
	values := map[string][]byte{}
	add := func(flag, pair string, read func(source string) ([]byte, error)) error {
		key, source, ok := strings.Cut(pair, "=")
		switch {
		case !ok:
			return usage("--%s %q: write <key>=...", flag, pair)
		case !names.ValidSecretKey(key):
			return usage("--%s: key %q: %s", flag, key, names.SecretKeyRule)
		case values[key] != nil:
			return usage("key %s is given twice", key)
		}
		value, err := read(source)
		if err != nil {
			return err
		}
		values[key] = value
		return nil
	}
	for _, pair := range literals {
		if err := add("from-literal", pair, func(value string) ([]byte, error) {
			if value == "" {
				return nil, errors.New("the value is empty")
			}
			return []byte(value), nil
		}); err != nil {
			return nil, err
		}
	}
	readStdin := false
	for _, pair := range fromFiles {
		if err := add("from-file", pair, func(path string) ([]byte, error) {
			if path != "-" {
				return readFileValue(path)
			}
			if readStdin {
				return nil, usage("--from-file: only one value comes from stdin")
			}
			readStdin = true
			return readValue(stdin, prompt)
		}); err != nil {
			return nil, err
		}
	}
	if len(values) == 0 {
		return nil, usage("give the values with --from-literal <key>=<value> or --from-file <key>=<path|->")
	}
	return values, nil
}

// readFileValue reads a value from a file, as it is.
func readFileValue(path string) ([]byte, error) {
	value, err := os.ReadFile(path)
	switch {
	case err != nil:
		return nil, fmt.Errorf("read the value: %w", err)
	case len(value) == 0:
		return nil, fmt.Errorf("%s is empty", path)
	}
	return value, nil
}

// secretName reads [<namespace>/]<name>. Without a namespace, it uses
// default, as get and delete do. names.System holds the cluster's own
// Secrets.
func secretName(text string) (namespace, name string, err error) {
	namespace, name, ok := strings.Cut(text, "/")
	if !ok {
		namespace, name = names.Default, text
	}
	switch {
	case namespace != names.System && !names.ValidNamespace(namespace):
		return "", "", usage("namespace %q: %s", namespace, names.NamespaceRule)
	case !names.Valid(name):
		return "", "", usage("secret %q: %s", name, names.Rule)
	}
	return namespace, name, nil
}

// clusterRecipients asks the cluster whom every value is encrypted for:
// its own key and the recovery key. A new cluster without a recovery key
// answers 409 and says what to do (api's errNoRecoveryKey).
func clusterRecipients(ctx context.Context, client *api.Client) ([]age.Recipient, error) {
	var encryption api.Encryption
	if err := client.Get(ctx, "/v1/encryption", &encryption); err != nil {
		return nil, err
	}
	var recipients []age.Recipient
	for _, text := range []string{encryption.Cluster, encryption.Recovery} {
		recipient, err := age.ParseX25519Recipient(text)
		if err != nil {
			return nil, fmt.Errorf("the cluster's recipients: %w", err)
		}
		recipients = append(recipients, recipient)
	}
	return recipients, nil
}

// readValue reads the value to encrypt. On a terminal, it asks for the
// value without echo. Otherwise it reads all of in, without one line end
// at the end, which echo and most files add.
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
