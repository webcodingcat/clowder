package cli

import (
	"fmt"
	"io"
	"os"

	"filippo.io/age"
	"github.com/spf13/cobra"

	"github.com/webcodingcat/clowder/internal/atomicfile"
	"github.com/webcodingcat/clowder/internal/secretvalue"
)

func newRekey(g *globals) *cobra.Command {
	var from, identityFile, certFile string
	command := &cobra.Command{
		Use:   "rekey -f <file|directory> --identity <file|-> [--cert <file>]",
		Short: "Seal the values of SealedSecrets again, for another cluster",
		Long: "Opens every value of the SealedSecrets in a file, or in the YAML files of a directory, with\n" +
			"the old cluster's sealing key (--identity: a file, or - for stdin) and seals it again for\n" +
			"the cluster of the context, or for the public sealing key in --cert. This is how the files\n" +
			"move to a new cluster when the old one is lost. Each value stays bound to its Secret. Only\n" +
			"the values change; comments and layout stay as they are.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if from == "" || identityFile == "" {
				return usage("give the old sealing key with --identity and the files with -f <file|directory>")
			}
			paths, err := filesOf(from, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			identities, err := readIdentities(identityFile, cmd.InOrStdin())
			if err != nil {
				return err
			}
			recipient, err := sealingRecipient(cmd, g, certFile)
			if err != nil {
				return err
			}
			for _, path := range paths {
				count, err := rekeyFile(path, identities, recipient)
				if err != nil {
					// each file is written whole or not at all, so the
					// files listed above are done, and this one and the
					// ones after it are not
					return fmt.Errorf("%w; the files listed above are rekeyed, the others not", err)
				}
				if count == 0 {
					skipped(cmd.ErrOrStderr(), path, "no SealedSecret")
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
	command.Flags().StringVar(&identityFile, "identity", "", "the old cluster's sealing key: a file, or - for stdin")
	command.Flags().StringVar(&certFile, "cert", "",
		"seal for the public sealing key in this file (clowderd sealing-key export --public), not the context's cluster")
	return command
}

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

// rekeyFile writes the file whole or not at all and returns how many values
// it rekeyed. Like an apply, it refuses a value that is not meant for its
// Secret.
func rekeyFile(path string, identities []age.Identity, recipient age.Recipient) (int, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	values, err := fileValues(path, content)
	if err != nil || len(values) == 0 {
		return 0, err
	}
	rekeyed := make([]string, len(values))
	for index, value := range values {
		opened, err := secretvalue.OpenFor(value.sealed, value.secret, identities...)
		if err != nil {
			return 0, fmt.Errorf("%s: %s: %w", path, value.label(), err)
		}
		if rekeyed[index], err = secretvalue.Seal(opened, recipient); err != nil {
			return 0, err
		}
	}
	rewritten, err := replaceValues(content, values, rekeyed)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", path, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return len(values), atomicfile.Write(path, rewritten, info.Mode().Perm())
}
