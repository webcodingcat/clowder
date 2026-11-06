package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/webcodingcat/clowder/internal/clientconfig"
	"github.com/webcodingcat/clowder/internal/pki"
)

// fromAHost tells the user how to get credentials. Messages that need to
// explain this use it.
const fromAHost = "ssh root@<host> clowderd credentials | clowder config import -"

func newConfig(g *globals) *cobra.Command {
	config := &cobra.Command{
		Use:   "config",
		Short: "The CLI's credentials file and its contexts (as kubectl config)",
	}
	config.AddCommand(newGetContexts(g), newUseContext(g), newDeleteContext(g), newImport(g))
	return config
}

// withHint adds advice to an error from picking or adding a context.
func withHint(err error) error {
	switch {
	case errors.Is(err, clientconfig.ErrNoCurrentContext):
		return fmt.Errorf("%w: pick one with clowder config use-context, or get credentials from a host: %s",
			err, fromAHost)
	case errors.Is(err, clientconfig.ErrNoContext):
		return fmt.Errorf("%w (clowder config get-contexts lists them)", err)
	case errors.Is(err, clientconfig.ErrContextTaken):
		return fmt.Errorf("%w: name the new one with --as, or replace the old one with --replace", err)
	}
	return err
}

func newGetContexts(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "get-contexts",
		Short: "List the contexts, the current one marked with *",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			config, _, err := g.readConfig()
			if err != nil {
				return err
			}
			return printContexts(cmd.OutOrStdout(), config)
		},
	}
}

// printContexts lists the contexts and when their certificates expire.
func printContexts(out io.Writer, config clientconfig.Config) error {
	table := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(table, "CURRENT\tNAME\tENDPOINTS\tCERTIFICATE-ENDS") // tabwriter reports errors on Flush
	for _, cliContext := range config.Contexts {
		current := ""
		if cliContext.Name == config.CurrentContext {
			current = "*"
		}
		ends := "unreadable"
		if certificate, err := pki.ParseCertificate([]byte(cliContext.Certificate)); err == nil {
			ends = certificate.NotAfter.Format(time.DateOnly)
		}
		_, _ = fmt.Fprintf(table, "%s\t%s\t%s\t%s\n",
			current, cliContext.Name, strings.Join(cliContext.Endpoints, ","), ends)
	}
	return table.Flush()
}

func newUseContext(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "use-context <name>",
		Short: "Make a context the current one",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return g.changeConfig(cmd.OutOrStdout(), "switched to context "+args[0], func(config *clientconfig.Config) error {
				return config.Use(args[0])
			})
		},
	}
}

func newDeleteContext(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "delete-context <name>",
		Short: "Remove a context from the credentials file, its key with it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return g.changeConfig(cmd.OutOrStdout(), "deleted context "+args[0], func(config *clientconfig.Config) error {
				return config.Delete(args[0])
			})
		},
	}
}

// importFlags are the flags of clowder config import.
type importFlags struct {
	as      string
	replace bool
}

func newImport(g *globals) *cobra.Command {
	var flags importFlags
	command := &cobra.Command{
		Use:   "import <file|->",
		Short: "Add the context of credentials from a host to your credentials file",
		Long: "Adds the context of a file that clowderd credentials printed (- for stdin) to your\n" +
			"credentials file, for example\n" +
			"  " + fromAHost + "\n" +
			"The context is named after the cluster unless --as gives another name. If the name is\n" +
			"already taken, choose another one, or use --replace to put the new credentials in place\n" +
			"of the old ones (for a renewed certificate). The first context becomes the current one.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			imported, err := readImport(cmd.InOrStdin(), args[0])
			if err != nil {
				return err
			}
			if len(imported.Contexts) != 1 {
				return fmt.Errorf("%s holds %d contexts, not the one clowderd credentials prints",
					args[0], len(imported.Contexts))
			}
			cliContext := imported.Contexts[0]
			if flags.as != "" {
				cliContext.Name = flags.as
			}
			add := func(config *clientconfig.Config) error { return config.Add(cliContext) }
			if flags.replace {
				add = func(config *clientconfig.Config) error { return config.Put(cliContext) }
			}
			return g.changeConfig(cmd.OutOrStdout(), "imported context "+cliContext.Name, add)
		},
	}
	// not --context, which picks the context that a command uses
	command.Flags().StringVar(&flags.as, "as", "", "the name of the new context (default: the cluster's)")
	command.Flags().BoolVar(&flags.replace, "replace", false, "replace a context of the same name")
	return command
}

// changeConfig reads the credentials file, changes it and writes it back.
// Then it prints what it did and which context is current.
func (g *globals) changeConfig(out io.Writer, done string, change func(config *clientconfig.Config) error) error {
	config, path, err := g.readConfig()
	if err != nil {
		return err
	}
	if err := change(&config); err != nil {
		return withHint(err)
	}
	if err := clientconfig.Write(path, config); err != nil {
		return err
	}
	current := config.CurrentContext
	if current == "" {
		current = "none"
	}
	_, err = fmt.Fprintf(out, "%s in %s (current: %s)\n", done, path, current)
	return err
}

// readImport reads the file to import, or stdin for "-".
func readImport(stdin io.Reader, path string) (clientconfig.Config, error) {
	var content []byte
	var err error
	if path == "-" {
		content, err = io.ReadAll(stdin)
	} else {
		content, err = os.ReadFile(path)
	}
	if err != nil {
		return clientconfig.Config{}, fmt.Errorf("read the credentials to import: %w", err)
	}
	config, err := clientconfig.Parse(content)
	if err != nil {
		return clientconfig.Config{}, fmt.Errorf("the credentials to import: %w", err)
	}
	return config, nil
}
