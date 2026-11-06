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

// The markers decrypt writes in place of a value.
const (
	errorMarker  = "error: "
	base64Marker = "base64:"
)

func newDecrypt() *cobra.Command {
	var from, identityFile string
	command := &cobra.Command{
		Use:   "decrypt --identity <file|-> -f <file>",
		Short: "In an emergency, read the values of a file with the recovery key",
		Long: "Decrypts every value tagged !secret in a file with the recovery key (a file, or - for\n" +
			"stdin) and prints one line per value: where the value is, a tab, and the value. Where is\n" +
			"a Secret's ID and the key (shop/db[password]). A value that is not printable text, that\n" +
			"starts or ends with a space, or that starts like one of the markers here is shown as\n" +
			"base64:... . A value that cannot be decrypted, or that is not meant for its place, is\n" +
			"shown as error: and the reason, the same one an apply would give; the command then ends\n" +
			"with an error after all lines. It needs no cluster.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if from == "" || identityFile == "" {
				return usage("give the recovery key with --identity and the file with -f")
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
				skipped(cmd.ErrOrStderr(), from, "no value tagged !secret")
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
	command.Flags().StringVar(&identityFile, "identity", "", "the recovery key: a file, or - for stdin")
	return command
}

// shownValue decrypts a value into the form decrypt prints: as it is if it
// is printable text, or else in base64. A space at either end would not be
// visible, and text that starts like a marker would read as a marker, so
// both are also shown in base64.
func shownValue(value fileValue, identities []age.Identity) (string, error) {
	opened, err := secretvalue.OpenFor(value.Encrypted, value.For, identities...)
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

// printable tells whether text prints on one line as it is: without line
// ends, tabs, other control characters, or spaces that a reader cannot
// tell apart from normal spaces.
func printable(text string) bool {
	return utf8.ValidString(text) && strings.IndexFunc(text, func(r rune) bool { return !unicode.IsPrint(r) }) < 0
}
