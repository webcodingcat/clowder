// Package agent is the clowderd process: the daemon (run) and the commands
// that run on a host (check, init; later join, recover). Only clowderd
// imports it.
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
	"github.com/webcodingcat/clowder/internal/network"
)

// NewRoot returns the clowderd command with all its subcommands.
func NewRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "clowderd",
		Short:         "Run and set up a clowder host",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(newVersion(), newCheck(), newInit(), newRun())
	return root
}

func newInit() *cobra.Command {
	var options InitOptions
	var underlay string
	var start bool
	initCommand := &cobra.Command{
		Use:   "init",
		Short: "Make this host the first of a new cluster (from cloud-init)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			address, err := netip.ParseAddr(underlay)
			if err != nil {
				return fmt.Errorf("--underlay: %w", err)
			}
			options.Underlay = address
			files := Files{Dir: DefaultDir}
			if err := Init(cmd.Context(), files, options, hostNetwork(), slog.Default()); err != nil {
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
	initCommand.Flags().StringVar(&options.BootstrapKey, "bootstrap-key", "", "the cluster's bootstrap key (from OpenTofu)")
	initCommand.Flags().StringVar(&options.Name, "name", hostname, "the host's name in the cluster")
	initCommand.Flags().StringVar(&underlay, "underlay", "", "the host's address in the network between the hosts")
	initCommand.Flags().BoolVar(&start, "start", true, "enable and start clowderd.service afterwards")
	_ = initCommand.MarkFlagRequired("bootstrap-key") // the flags exist; this cannot fail
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
			return Run(ctx, Files{Dir: DefaultDir}, hostNetwork(), slog.Default())
		},
	}
}

// hostNetwork is the host's real network.
func hostNetwork() network.Host {
	return network.Host{ResolvConf: "/etc/resolv.conf", Logger: slog.Default()}
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
