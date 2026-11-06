package cli

import (
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"

	"filippo.io/age"
	"github.com/spf13/cobra"

	"github.com/webcodingcat/clowder/internal/secretvalue"
)

// The markers that decrypt writes in place of a value.
const (
	errorMarker  = "error: "
	base64Marker = "base64:"
)

func newDecrypt() *cobra.Command {
	var from, identityFile string
	command := &cobra.Command{
		Use:   "decrypt --identity <file|-> -f <file>",
		Short: "In an emergency, read the values of SealedSecrets with the sealing key",
		Long: "Opens every value of the SealedSecrets in a file with the cluster's sealing key (a file, or\n" +
			"- for stdin; clowderd sealing-key export wrote it) and prints one line per value: where the\n" +
			"value is, a tab, and the value. Where is a Secret's ID and the key (shop/db[password]). A\n" +
			"value that is not printable text, that starts or ends with a space, or that starts like one\n" +
			"of the markers here is shown as base64:... . A value that cannot be opened, or that is not\n" +
			"meant for its Secret, is shown as error: and the reason, the same one an apply would give;\n" +
			"the command then ends with an error after all lines. It needs no cluster.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if from == "" || identityFile == "" {
				return usage("give the sealing key with --identity and the file with -f")
			}
			identities, err := readIdentities(identityFile, cmd.InOrStdin())
			if err != nil {
				return err
			}
			content, err := os.ReadFile(from)
			if err != nil {
				return err
			}
			values, err := fileValues(from, content)
			if err != nil {
				return err
			}
			if len(values) == 0 {
				skipped(cmd.ErrOrStderr(), from, "no SealedSecret")
			}
			failed := 0
			for _, value := range values {
				shown, err := shownValue(value, identities)
				if err != nil {
					failed++
					shown = errorMarker + err.Error()
				}
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\n", value.label(), shown); err != nil {
					return err
				}
			}
			if failed > 0 {
				return fmt.Errorf("%s: %d of %d values did not open", from, failed, len(values))
			}
			return nil
		},
	}
	command.Flags().StringVarP(&from, "filename", "f", "", "the file")
	command.Flags().StringVar(&identityFile, "identity", "", "the sealing key: a file, or - for stdin")
	return command
}

// shownValue opens a value and returns it in the form that decrypt prints:
// unchanged if it is printable text, or else in base64. A space at either
// end would not be visible, and text that starts like a marker would read
// as a marker. So both are shown in base64 too.
func shownValue(value fileValue, identities []age.Identity) (string, error) {
	opened, err := secretvalue.OpenFor(value.sealed, value.secret, identities...)
	if err != nil {
		return "", err
	}
	text := string(opened.Bytes)
	plain := printable(text) && strings.TrimSpace(text) == text &&
		!strings.HasPrefix(text, errorMarker) && !strings.HasPrefix(text, base64Marker)
	if plain {
		return text, nil
	}
	return base64Marker + base64.StdEncoding.EncodeToString(opened.Bytes), nil
}

// printable tells whether text prints unchanged on one line. That means no
// line breaks, tabs or other control characters, and no spaces that a
// reader cannot tell apart from normal spaces.
func printable(text string) bool {
	return utf8.ValidString(text) && strings.IndexFunc(text, func(r rune) bool { return !unicode.IsPrint(r) }) < 0
}
