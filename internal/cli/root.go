// Package cli holds the commands of the clowder CLI. It must build on
// Linux, macOS and Windows, so no package that only works on Linux may be
// imported here.
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
	root.AddCommand(newVersion(), newConfig(&shared), newCluster(&shared), newSecret(&shared),
		newHost(&shared), newStack(&shared), newVolume(&shared), newAlert(&shared),
		newApplyResources(&shared), newCreate(&shared), newDiff(&shared), newGet(&shared), newDelete(&shared))
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageError{err} })
	checkArgs(root)
	return root
}

// usageError is a wrong call: an unknown command, or flags or arguments
// that do not fit. clowder ends with exit code 2 for it.
type usageError struct{ err error }

func (e usageError) Error() string { return e.err.Error() }

func (e usageError) Unwrap() error { return e.err }

func usage(format string, args ...any) error {
	return usageError{fmt.Errorf(format, args...)}
}

// checkArgs turns every command's argument errors into usage errors, and
// gives each command that only groups others a RunE. Without it, cobra
// would show the help and exit as if it had succeeded.
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

// ExitCode returns the exit code for the error that a command returned: 2
// for a wrong call or when diff could not tell, and 1 for anything else.
func ExitCode(err error) int {
	var wrongCall usageError
	var failed diffFailed
	switch {
	case err == nil:
		return 0
	case errors.As(err, &wrongCall), errors.As(err, &failed):
		return 2
	default:
		return 1
	}
}

// Silent tells whether the error is an answer rather than a fault. clowder
// prints nothing for such an error. diff's errDiffers is one.
func Silent(err error) bool {
	return errors.Is(err, errDiffers)
}

// diffFailed is diff's error when it could not tell whether the resources
// differ.
type diffFailed struct{ err error }

func (e diffFailed) Error() string { return e.err.Error() }

func (e diffFailed) Unwrap() error { return e.err }

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
