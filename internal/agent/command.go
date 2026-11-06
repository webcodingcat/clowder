// Package agent is the clowderd process: the daemon (run) and the commands
// that run on a host (check, init, join, credentials, sealing-key,
// recover).
// Only clowderd imports it.
package agent

import (
	"context"
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
	"github.com/webcodingcat/clowder/internal/hostrole"
	"github.com/webcodingcat/clowder/internal/jointoken"
	"github.com/webcodingcat/clowder/internal/network"
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
	root.AddCommand(newVersion(), newCheck(), newInit(), newJoin(), newRun(), newCredentials(), newSealingKey(),
		newRecover())
	return root
}

// hostFlags are the flags that init and join share. They describe the host.
// The host's name in the cluster is its hostname, which DRBD also uses for
// the host in a resource.
type hostFlags struct {
	underlay, publicAddress, roles string
	serverID                       int64
	start                          bool
}

// add adds the flags; roles is the default of --roles.
func (f *hostFlags) add(cmd *cobra.Command, roles hostrole.Set) {
	cmd.Flags().StringVar(&f.roles, "roles", roles.String(), "the host's roles: "+hostrole.Rule)
	cmd.Flags().StringVar(&f.underlay, "underlay", "", "the server's address in the Hetzner private network")
	cmd.Flags().StringVar(&f.publicAddress, "public-address", "", "the server's public IPv4, where users reach the API")
	cmd.Flags().Int64Var(&f.serverID, "server-id", 0, "the Hetzner Cloud ID of this server")
	cmd.Flags().BoolVar(&f.start, "start", true, "enable and start clowderd.service afterwards")
	for _, name := range []string{"underlay", "public-address", "server-id"} {
		_ = cmd.MarkFlagRequired(name) // the flags exist; this cannot fail
	}
}

// parsedHost holds the values that hostFlags.parse returns.
type parsedHost struct {
	name                    string
	roles                   hostrole.Set
	underlay, publicAddress netip.Addr
}

// parse reads the hostname, the roles and the addresses.
func (f *hostFlags) parse() (parsedHost, error) {
	var host parsedHost
	var err error
	if host.name, err = os.Hostname(); err != nil {
		return host, fmt.Errorf("read the hostname, the host's name in the cluster: %w", err)
	}
	if host.roles, err = hostrole.Parse(f.roles); err != nil {
		return host, fmt.Errorf("--roles: %w", err)
	}
	if host.underlay, err = netip.ParseAddr(f.underlay); err != nil {
		return host, fmt.Errorf("--underlay: %w", err)
	}
	if host.publicAddress, err = netip.ParseAddr(f.publicAddress); err != nil {
		return host, fmt.Errorf("--public-address: %w", err)
	}
	return host, nil
}

// startService enables and starts clowderd.service. From then on the agent
// runs as a service and survives reboots.
func (f *hostFlags) startService(ctx context.Context) error {
	if !f.start {
		return nil
	}
	if _, err := command.Run(ctx, time.Minute, "systemctl", "enable", "--now", "clowderd.service"); err != nil {
		return fmt.Errorf("start clowderd.service: %w", err)
	}
	return nil
}

func newInit() *cobra.Command {
	var flags hostFlags
	var clusterName string
	initCommand := &cobra.Command{
		Use:   "init",
		Short: "Make this host the first of a new cluster (from cloud-init)",
		Long: "Makes this host the first of a new cluster. The first credentials come from root on it\n" +
			"afterwards: ssh root@<host> clowderd credentials | clowder config import -\n" +
			"Then export the cluster's sealing key and keep it offline: clowderd sealing-key export",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			host, err := flags.parse()
			if err != nil {
				return err
			}
			options := InitOptions{
				ClusterName: clusterName, Name: host.name, Roles: host.roles, Underlay: host.underlay,
				PublicAddress: host.publicAddress, ServerID: flags.serverID,
			}
			files := Files{Dir: DefaultDir}
			if err := Init(cmd.Context(), files, options, hostNetwork(files), slog.Default()); err != nil {
				return err
			}
			return flags.startService(cmd.Context())
		},
	}
	flags.add(initCommand, hostrole.DefaultInit)
	initCommand.Flags().StringVar(&clusterName, "cluster-name", DefaultClusterName,
		"the cluster's name: cluster status shows it, alerts and new credentials carry it")
	return initCommand
}

func newJoin() *cobra.Command {
	var flags hostFlags
	var via, tokenFile string
	joinCommand := &cobra.Command{
		Use:   "join",
		Short: "Make this host a member of a cluster, with a join token (from cloud-init)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			host, err := flags.parse()
			if err != nil {
				return err
			}
			options := JoinOptions{
				Via: via, Name: host.name, Roles: host.roles, Underlay: host.underlay,
				PublicAddress: host.publicAddress, ServerID: flags.serverID,
			}
			content, err := os.ReadFile(tokenFile)
			if err != nil {
				return fmt.Errorf("read the join token: %w", err)
			}
			if options.Token, err = jointoken.Parse(string(content)); err != nil {
				return err
			}
			files := Files{Dir: DefaultDir}
			if err := Join(cmd.Context(), files, options, hostNetwork(files), slog.Default()); err != nil {
				return err
			}
			return flags.startService(cmd.Context())
		},
	}
	flags.add(joinCommand, hostrole.DefaultJoin)
	joinCommand.Flags().StringVar(&via, "via", "", "an agent of the cluster to join through: its underlay address")
	joinCommand.Flags().StringVar(&tokenFile, "token-file", "",
		"a file with the join token (clowder host join-token); not a flag of its own, which /proc would show")
	_ = joinCommand.MarkFlagRequired("via") // the flags exist; this cannot fail
	_ = joinCommand.MarkFlagRequired("token-file")
	return joinCommand
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

// hostStorage is the host's real storage driver.
func hostStorage(files Files) storage.Driver {
	return storage.Driver{
		VolumesDir: files.volumesDir(), StateDir: files.storageDir(), ResourceDir: "/etc/drbd.d", Run: command.Run,
	}
}

// hostParts are the host's real network, storage and Docker.
func hostParts(files Files) HostParts {
	return HostParts{
		Network: hostNetwork(files),
		Storage: hostStorage(files),
		Docker:  docker.NewClient(docker.Socket),
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
