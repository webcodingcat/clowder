package cli

import (
	"encoding/base64"
	"encoding/json"
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
	secret := &cobra.Command{Use: "secret", Short: "SealedSecrets: values sealed for the cluster"}
	secret.AddCommand(newSeal(g), newRekey(g), newDecrypt())
	return secret
}

// valueFlags are like those of kubectl create secret generic.
type valueFlags struct {
	literals, fromFiles []string
}

func (f *valueFlags) register(command *cobra.Command) {
	command.Flags().StringArrayVar(&f.literals, "from-literal", nil, "a key and its value, <key>=<value>")
	command.Flags().StringArrayVar(&f.fromFiles, "from-file", nil,
		"a key and the file of its value, <key>=<path>, or <key>=- for stdin")
}

const valueFlagsHelp = "--from-file takes a file's content, or with - one value from stdin: on a terminal it asks\n" +
	"without echo, otherwise it reads stdin without one line end at the end. --from-literal puts\n" +
	"the value on the command line, where the shell's history keeps it."

func newSeal(g *globals) *cobra.Command {
	var from, certFile string
	var flags valueFlags
	command := &cobra.Command{
		Use:   "seal (-f <file|-> | [<namespace>/]<name> (--from-literal <key>=<value> | --from-file <key>=<path|->)...)",
		Short: "Seal a Secret's values for the cluster and print the SealedSecret",
		Long: "Seals the values of Secrets for the cluster's sealing key and prints the SealedSecrets to\n" +
			"apply, as kubeseal does. The values come from the Secrets of a file (-f, or - for stdin),\n" +
			"or from the flags for one Secret (in default if no namespace is given), which works like\n" +
			"kubectl create secret generic. " + valueFlagsHelp + "\n" +
			"Each value is bound to its Secret, and no other Secret accepts it. Only the cluster, or\n" +
			"whoever holds the sealing key, can open the values; a SealedSecret may go into Git. The\n" +
			"public sealing key comes from the cluster each time, or from --cert without a cluster.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			fromFlags := len(flags.literals)+len(flags.fromFiles) > 0
			var secrets []resource.Object
			var err error
			switch {
			case from != "" && (len(args) > 0 || fromFlags):
				return usage("give either -f or a name with --from-literal and --from-file, not both")
			case from != "":
				secrets, err = readSecrets(from, cmd.InOrStdin(), cmd.ErrOrStderr())
			case len(args) == 1:
				var secret resource.Object
				secret, err = secretFromFlags(args[0], flags, cmd.InOrStdin(), cmd.ErrOrStderr())
				secrets = []resource.Object{secret}
			default:
				return usage("give the Secrets with -f, or a name with --from-literal and --from-file")
			}
			if err != nil {
				return err
			}
			recipient, err := sealingRecipient(cmd, g, certFile)
			if err != nil {
				return err
			}
			var out strings.Builder
			for index, secret := range secrets {
				if index > 0 {
					out.WriteString("---\n")
				}
				sealed, err := sealedSecret(secret, recipient)
				if err != nil {
					return err
				}
				out.WriteString(sealed)
			}
			_, err = fmt.Fprint(cmd.OutOrStdout(), out.String())
			return err
		},
	}
	command.Flags().StringVarP(&from, "filename", "f", "", "a file of Secrets, or - for stdin")
	command.Flags().StringVar(&certFile, "cert", "",
		"seal for the public sealing key in this file (clowderd sealing-key export --public), without a cluster")
	flags.register(command)
	return command
}

// readSecrets refuses a resource of another kind, because seal would drop
// it.
func readSecrets(from string, stdin io.Reader, report io.Writer) ([]resource.Object, error) {
	objects, err := readResourceFiles(from, stdin, report)
	if err != nil {
		return nil, err
	}
	for _, object := range objects {
		if object.Kind != resource.KindSecret {
			return nil, fmt.Errorf("%s: seal takes Secrets only", object.Ref())
		}
	}
	return objects, nil
}

func secretFromFlags(text string, flags valueFlags, stdin io.Reader, prompt io.Writer) (resource.Object, error) {
	namespace, name, err := secretName(text)
	if err != nil {
		return resource.Object{}, err
	}
	values, err := secretValues(flags, stdin, prompt)
	if err != nil {
		return resource.Object{}, err
	}
	// data, not stringData: a file's value may hold any bytes, and JSON
	// would change bytes that are not valid UTF-8
	spec := resource.SecretSpec{Data: map[string]string{}}
	for key, value := range values {
		spec.Data[key] = base64.StdEncoding.EncodeToString(value)
	}
	encoded, err := json.Marshal(spec)
	if err != nil {
		return resource.Object{}, fmt.Errorf("encode the secret: %w", err)
	}
	return resource.Object{
		APIVersion: resource.APIVersion, Kind: resource.KindSecret,
		Metadata: resource.Metadata{Namespace: namespace, Name: name}, Spec: encoded,
	}, nil
}

// sealedSecret writes each value on one line, as kubeseal does.
func sealedSecret(secret resource.Object, recipient age.Recipient) (string, error) {
	values, err := resource.DecodeSecretSpec(secret.Spec)
	if err != nil {
		return "", fmt.Errorf("%s: %w", secret.Ref(), err)
	}
	if err := checkSecretName(secret.Metadata.Namespace, secret.Metadata.Name); err != nil {
		return "", fmt.Errorf("%s: %w", secret.Ref(), err)
	}
	id := names.ID(secret.Metadata.Namespace, secret.Metadata.Name)
	// names and keys hold only letters, digits, -, _ and . (checked above
	// and by DecodeSecretSpec), which YAML accepts without quotes
	var out strings.Builder
	fmt.Fprintf(&out, "apiVersion: %s\nkind: %s\nmetadata: {namespace: %s, name: %s}\nspec:\n  encryptedData:\n",
		resource.APIVersion, resource.KindSealedSecret, secret.Metadata.Namespace, secret.Metadata.Name)
	for _, key := range slices.Sorted(maps.Keys(values)) {
		sealed, err := secretvalue.Seal(secretvalue.Value{For: id, Bytes: values[key]}, recipient)
		if err != nil {
			return "", err
		}
		out.WriteString("    " + key + ": " + sealed + "\n")
	}
	return out.String(), nil
}

// sealingRecipient asks the cluster each time unless certFile is given,
// and keeps no copy that someone could change.
func sealingRecipient(cmd *cobra.Command, g *globals, certFile string) (age.Recipient, error) {
	var text string
	if certFile != "" {
		content, err := os.ReadFile(certFile)
		if err != nil {
			return nil, fmt.Errorf("read the public sealing key: %w", err)
		}
		text = strings.TrimSpace(string(content))
	} else {
		client, err := g.connect()
		if err != nil {
			return nil, err
		}
		var public api.PublicSealingKey
		if err := client.Get(cmd.Context(), "/v1/sealing-key", &public); err != nil {
			return nil, err
		}
		text = public.Recipient
	}
	recipient, err := age.ParseX25519Recipient(text)
	if err != nil {
		return nil, fmt.Errorf("the public sealing key: %w", err)
	}
	return recipient, nil
}

// secretValues allows each key once, and stdin at most once.
func secretValues(flags valueFlags, stdin io.Reader, prompt io.Writer) (map[string][]byte, error) {
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
	for _, pair := range flags.literals {
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
	for _, pair := range flags.fromFiles {
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

// readFileValue keeps a trailing line break, unlike readValue.
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

// secretName reads [<namespace>/]<name>, with default as get and delete
// use it.
func secretName(text string) (namespace, name string, err error) {
	namespace, name, ok := strings.Cut(text, "/")
	if !ok {
		namespace, name = names.Default, text
	}
	if err := checkSecretName(namespace, name); err != nil {
		return "", "", err
	}
	return namespace, name, nil
}

// checkSecretName also accepts names.System, which holds the cluster's own
// Secrets.
func checkSecretName(namespace, name string) error {
	switch {
	case namespace != names.System && !names.ValidNamespace(namespace):
		return usage("namespace %q: %s", namespace, names.NamespaceRule)
	case !names.Valid(name):
		return usage("secret %q: %s", name, names.Rule)
	}
	return nil
}

// readValue asks without echo on a terminal. Otherwise it drops one
// trailing line break, which echo and most files add.
func readValue(in io.Reader, prompt io.Writer) ([]byte, error) {
	var value []byte
	if file, ok := in.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
		if _, err := fmt.Fprint(prompt, "value: "); err != nil {
			return nil, err
		}
		read, err := term.ReadPassword(int(file.Fd()))
		_, _ = fmt.Fprintln(prompt) // the hidden input showed no line break
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
