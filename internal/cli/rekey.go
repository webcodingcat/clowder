package cli

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/webcodingcat/clowder/internal/api"
	"github.com/webcodingcat/clowder/internal/atomicfile"
	"github.com/webcodingcat/clowder/internal/secretvalue"
)

// rekeyFunc turns values (secretvalue's armor) into the same values
// encrypted for the cluster's current recipients, in the same order. It
// refuses a value that is not meant for its place.
type rekeyFunc func(ctx context.Context, values []api.PlacedValue) ([]string, error)

// rekeyBatchBytes is how much armor goes into one request: half the limit,
// because the JSON around the values and its escapes come on top.
const rekeyBatchBytes = api.MaxRequestBytes / 2

func newRekey(g *globals) *cobra.Command {
	var from, identityFile string
	command := &cobra.Command{
		Use:   "rekey -f <file|directory> [--identity <file|->]",
		Short: "Encrypt the values in files again, for the cluster's recipients",
		Long: "Encrypts every value tagged !secret in a file, or in the YAML files of a directory, again\n" +
			"for the cluster's key and the recovery key. Each value stays bound to its place. After\n" +
			"that, the recovery key decrypts every value, even one made by hand with age. The cluster\n" +
			"decrypts the values and answers only with ciphertext. With --identity, the CLI decrypts\n" +
			"them itself with the recovery key (a file, or - for stdin) and encrypts them for the\n" +
			"cluster of the context; this is how values move to a new cluster after the old one is\n" +
			"lost. Only the lines of the values change; comments and layout stay as they are.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if from == "" {
				return usage("name the files with -f <file|directory>")
			}
			paths, err := filesOf(from, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			client, err := g.connect()
			if err != nil {
				return err
			}
			var rekey rekeyFunc
			if identityFile == "" {
				rekey = rekeyByCluster(client, rekeyBatchBytes)
			} else {
				rekey, err = rekeyByIdentity(cmd, client, identityFile)
				if err != nil {
					return err
				}
			}
			for _, path := range paths {
				count, err := rekeyFile(cmd.Context(), path, rekey)
				if err != nil {
					// each file is written whole or not at all: the files
					// listed above are done, this one and the ones after it
					// are not
					return fmt.Errorf("%w; the files listed above are rekeyed, the others not", err)
				}
				if count == 0 {
					skipped(cmd.ErrOrStderr(), path, "no value tagged !secret")
					continue
				}
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s: %d values rekeyed\n", path, count); err != nil {
					return err
				}
			}
			return nil
		},
	}
	command.Flags().StringVarP(&from, "filename", "f", "", "a file or a directory of files")
	command.Flags().StringVar(&identityFile, "identity", "", "the recovery key: a file, or - for stdin")
	return command
}

// filesOf returns a file, or the YAML files of a directory.
func filesOf(from string, report io.Writer) ([]string, error) {
	info, err := os.Stat(from)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return []string{from}, nil
	}
	return yamlFiles(from, report)
}

// rekeyByCluster has the cluster rekey the values, in requests of at most
// batchBytes of armor, because a file may hold more values than one
// request can carry.
func rekeyByCluster(client *api.Client, batchBytes int) rekeyFunc {
	return func(ctx context.Context, values []api.PlacedValue) ([]string, error) {
		var rekeyed []string
		start := 0 // the first value of the next request
		for start < len(values) {
			// at least one value per request, so the loop moves on; the
			// cluster refuses a value larger than one request can carry
			end, size := start+1, len(values[start].Encrypted)
			for end < len(values) && size+len(values[end].Encrypted) <= batchBytes {
				size += len(values[end].Encrypted)
				end++
			}
			answer, err := client.Rekey(ctx, values[start:end])
			switch {
			case err != nil && start == 0 && end == len(values):
				return nil, err
			case err != nil:
				// the cluster counts the values of its request only
				return nil, fmt.Errorf("the request with values %d to %d: %w", start+1, end, err)
			}
			rekeyed = append(rekeyed, answer...)
			start = end
		}
		return rekeyed, nil
	}
}

// rekeyByIdentity decrypts the values locally with the recovery key and
// encrypts them for the recipients that the cluster names. This is how
// values move to a new cluster when the old one, whose key decrypted them,
// is lost.
func rekeyByIdentity(cmd *cobra.Command, client *api.Client, identityFile string) (rekeyFunc, error) {
	identities, err := readIdentities(identityFile, cmd.InOrStdin())
	if err != nil {
		return nil, err
	}
	recipients, err := clusterRecipients(cmd.Context(), client)
	if err != nil {
		return nil, err
	}
	return func(_ context.Context, values []api.PlacedValue) ([]string, error) {
		rekeyed := make([]string, len(values))
		for index, value := range values {
			opened, err := secretvalue.OpenFor(value.Encrypted, value.For, identities...)
			if err != nil {
				return nil, fmt.Errorf("value %d: %w", index+1, err)
			}
			if rekeyed[index], err = secretvalue.Encrypt(opened, recipients...); err != nil {
				return nil, err
			}
		}
		return rekeyed, nil
	}, nil
}

// rekeyFile rekeys the values of one file and writes it back whole or not
// at all, with only the lines of the values changed. It tells how many
// values it rekeyed. Every file with a value is written: age encrypts with
// a fresh key each time, so new armor never equals the old, and the
// ciphertext does not tell whether a value is already encrypted for the
// recovery key.
func rekeyFile(ctx context.Context, path string, rekey rekeyFunc) (int, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	values, err := fileValues(path, content)
	if err != nil || len(values) == 0 {
		return 0, err
	}
	placed := make([]api.PlacedValue, len(values))
	for index, value := range values {
		placed[index] = value.PlacedValue
	}
	rekeyed, err := rekey(ctx, placed)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", path, err)
	}
	// by place: within a document, Rewrite meets the values in the file's
	// order, fileValues lists them by path
	rekeyedAt := map[valuePlace]string{}
	for index, value := range values {
		rekeyedAt[value.place] = rekeyed[index]
	}
	rewritten, err := secretvalue.Rewrite(content, func(document int, valuePath, _ string) (string, error) {
		armored, ok := rekeyedAt[valuePlace{document, valuePath}]
		if !ok {
			return "", fmt.Errorf("document %d: fileValues did not find this value", document+1)
		}
		return armored, nil
	})
	if err != nil {
		return 0, fmt.Errorf("%s: %w", path, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return len(values), atomicfile.Write(path, rewritten, info.Mode().Perm())
}
