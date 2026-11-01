// Package cli holds the commands of the clowder CLI. It must build on Linux,
// macOS and Windows: no package that only works on Linux may be imported here
// (.golangci.yml checks the direct imports, the cross build in CI the rest).
package cli

import (
	"errors"
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
	var shared globals
	shared.register(root)
	root.AddCommand(newVersion(), newCluster(&shared), newSecret(&shared), newHost(&shared), newStack(&shared), newAlert(&shared),
		newApplyResources(&shared), newGet(&shared), newDelete(&shared))
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageError{err} })
	checkArgs(root)
	return root
}

// usageError is a wrong call: an unknown command, a flag or arguments that
// do not fit. clowder ends with exit code 2 for it.
type usageError struct{ err error }

func (e usageError) Error() string { return e.err.Error() }

func (e usageError) Unwrap() error { return e.err }

// usage makes a wrong call of an error.
func usage(format string, args ...any) error {
	return usageError{fmt.Errorf(format, args...)}
}

// checkArgs makes the argument errors of every command usage errors. A
// command of commands (clowder, clowder stack) shows its help, and takes
// an unknown command as a wrong call: without this, cobra would show the
// help and end as if done.
func checkArgs(command *cobra.Command) {
	if command.HasSubCommands() && !command.Runnable() {
		command.Args = cobra.ArbitraryArgs
		command.RunE = func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return usage("unknown command %q for %q", args[0], cmd.CommandPath())
			}
			return cmd.Help()
		}
	}
	if check := command.Args; check != nil {
		command.Args = func(cmd *cobra.Command, args []string) error {
			if err := check(cmd, args); err != nil {
				return usageError{err}
			}
			return nil
		}
	}
	for _, child := range command.Commands() {
		checkArgs(child)
	}
}

// ExitCode is the exit code for the error a command returned: 0 when it
// did what was asked, 2 for a wrong call, 1 for anything else (refused,
// missing, not reachable).
func ExitCode(err error) int {
	var wrongCall usageError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &wrongCall):
		return 2
	default:
		return 1
	}
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
