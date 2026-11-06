// Package clowderd wires the clowderd process together: the daemon (run)
// and the commands that run on a host (check, init, join, credentials,
// sealing-key, recover), each in a package of its own.
// Only cmd/clowderd imports it.
package clowderd

import (
	"fmt"
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/webcodingcat/clowder/internal/buildinfo"
	"github.com/webcodingcat/clowder/internal/clowderd/check"
	"github.com/webcodingcat/clowder/internal/clowderd/credentials"
	"github.com/webcodingcat/clowder/internal/clowderd/daemon"
	"github.com/webcodingcat/clowder/internal/clowderd/hostfiles"
	"github.com/webcodingcat/clowder/internal/clowderd/initialize"
	"github.com/webcodingcat/clowder/internal/clowderd/join"
	"github.com/webcodingcat/clowder/internal/clowderd/recovery"
	"github.com/webcodingcat/clowder/internal/clowderd/sealingkey"
	"github.com/webcodingcat/clowder/internal/command"
	"github.com/webcodingcat/clowder/internal/docker"
	"github.com/webcodingcat/clowder/internal/network"
	"github.com/webcodingcat/clowder/internal/storage"
)

// NewRoot returns the clowderd command with all its subcommands. It builds
// the host's files and drivers once and hands each command what it needs.
func NewRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "clowderd",
		Short:         "Run and set up a clowder host",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	files := hostfiles.Default()
	root.AddCommand(
		newVersion(),
		check.NewCommand(),
		initialize.NewCommand(files, hostNetwork(files)),
		join.NewCommand(files, hostNetwork(files)),
		daemon.NewCommand(files, hostParts(files)),
		credentials.NewCommand(files),
		sealingkey.NewCommand(files),
		recovery.NewCommand(files, hostStorage(files)),
	)
	return root
}

func hostNetwork(files hostfiles.Files) network.Host {
	return network.Host{
		ResolvConf: "/etc/resolv.conf", WireGuardKey: files.WireGuardKey(), Run: command.RunWithInput,
		Logger: slog.Default(),
	}
}

func hostStorage(files hostfiles.Files) storage.Driver {
	return storage.Driver{
		VolumesDir: files.VolumesDir(), StateDir: files.StorageDir(), ResourceDir: "/etc/drbd.d", Run: command.Run,
	}
}

func hostParts(files hostfiles.Files) daemon.HostParts {
	return daemon.HostParts{
		Network: hostNetwork(files),
		Storage: hostStorage(files),
		Docker:  docker.NewClient(docker.Socket),
	}
}

// newVersion stays here: it belongs to no command package, and it needs
// neither the host's files nor its drivers.
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
