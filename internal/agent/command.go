// Package agent is the clowderd process: the daemon (run) and the commands
// that run on a host (check, init, user recovery-code; later join,
// recover). Only clowderd imports it.
package agent

import (
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/webcodingcat/clowder/internal/buildinfo"
	"github.com/webcodingcat/clowder/internal/command"
	"github.com/webcodingcat/clowder/internal/docker"
	"github.com/webcodingcat/clowder/internal/hoster"
	"github.com/webcodingcat/clowder/internal/network"
	"github.com/webcodingcat/clowder/internal/pki"
	"github.com/webcodingcat/clowder/internal/storage"
)

// NewRoot returns the clowderd command with all its subcommands.
func NewRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "clowderd",
		Short:         "Run and set up a clowder host",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(newVersion(), newCheck(), newInit(), newRun(), newUser())
	return root
}

func newInit() *cobra.Command {
	var options InitOptions
	var underlay, publicAddress, keyFile string
	var start bool
	initCommand := &cobra.Command{
		Use:   "init",
		Short: "Make this host the first of a new cluster (from cloud-init)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var err error
			if options.Underlay, err = netip.ParseAddr(underlay); err != nil {
				return fmt.Errorf("--underlay: %w", err)
			}
			if publicAddress != "" {
				if options.PublicAddress, err = netip.ParseAddr(publicAddress); err != nil {
					return fmt.Errorf("--public-address: %w", err)
				}
			}
			if options.BootstrapKey, err = pki.ReadBootstrapKey(keyFile); err != nil {
				return err
			}
			files := Files{Dir: DefaultDir}
			if err := Init(cmd.Context(), files, options, hostNetwork(files), slog.Default()); err != nil {
				return err
			}
			if !start {
				return nil
			}
			// the agent runs as a service from now on, across reboots
			if _, err := command.Run(cmd.Context(), time.Minute, "systemctl", "enable", "--now",
				"clowderd.service"); err != nil {
				return fmt.Errorf("start clowderd.service: %w", err)
			}
			return nil
		},
	}
	hostname, _ := os.Hostname() // without one, --name is needed
	initCommand.Flags().StringVar(&keyFile, "bootstrap-key-file", "",
		"a file with the cluster's bootstrap key (from OpenTofu); not a flag of its own, which /proc would show")
	initCommand.Flags().StringVar(&options.Name, "name", hostname, "the host's name in the cluster")
	initCommand.Flags().StringVar(&underlay, "underlay", "", "the host's address in the network between the hosts")
	initCommand.Flags().StringVar(&publicAddress, "public-address", "",
		"where users reach the API, if the underlay is a private network")
	initCommand.Flags().StringVar(&options.ServerRef, "server-ref", "", "the hoster's ID of this server")
	initCommand.Flags().BoolVar(&start, "start", true, "enable and start clowderd.service afterwards")
	_ = initCommand.MarkFlagRequired("bootstrap-key-file") // the flags exist; this cannot fail
	_ = initCommand.MarkFlagRequired("underlay")
	return initCommand
}

func newRun() *cobra.Command {
	return &cobra.Command{
		Use:   "run",
		Short: "Run the agent (clowderd.service)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGTERM, os.Interrupt)
			defer stop()
			files := Files{Dir: DefaultDir, SecretsDir: DefaultSecretsDir}
			return Run(ctx, files, hostParts(files), slog.Default())
		},
	}
}

// hostNetwork is the host's real network.
func hostNetwork(files Files) network.Host {
	return network.Host{
		ResolvConf: "/etc/resolv.conf", WireGuardKey: files.wireGuardKey(), Run: command.RunWithInput,
		Logger: slog.Default(),
	}
}

// hostParts are the host's real network, storage and Docker, and the
// hoster's real API.
func hostParts(files Files) HostParts {
	return HostParts{
		Network: hostNetwork(files),
		Storage: storage.Driver{
			VolumesDir:  files.volumesDir(),
			StateDir:    files.storageDir(),
			ResourceDir: "/etc/drbd.d",
			Run:         command.Run,
		},
		Docker: docker.NewClient(docker.Socket),
		Hoster: hoster.New,
	}
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
