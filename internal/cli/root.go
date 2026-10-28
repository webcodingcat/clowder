// Package cli holds the commands of the clowder CLI. It must build on Linux,
// macOS and Windows: no package that only works on Linux may be imported here
// (.golangci.yml checks the direct imports, the cross build in CI the rest).
package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/webcodingcat/clowder/internal/buildinfo"
)

// NewRoot returns the clowder command with all its subcommands.
func NewRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "clowder",
		Short:         "Manage a clowder cluster",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(newVersion())
	return root
}

func newVersion() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version of this CLI",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), "clowder", buildinfo.Version)
			return err
		},
	}
}
