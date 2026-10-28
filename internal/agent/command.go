// Package agent is the clowderd process: the daemon and the commands that run
// on a host (check, init, join). Only clowderd imports it.
package agent

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/webcodingcat/clowder/internal/buildinfo"
)

// NewRoot returns the clowderd command with all its subcommands.
func NewRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "clowderd",
		Short:         "Run and set up a clowder host",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(newVersion(), newCheck())
	return root
}

func newVersion() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version of this agent",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), "clowderd", buildinfo.Version)
			return err
		},
	}
}
