package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/webcodingcat/clowder/internal/api"
)

func printHosts(out io.Writer, hosts []api.HostInfo) error {
	table := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(table, "ID\tNAME\tPHASE\tVOTER\tUNDERLAY\tPUBLIC-ADDRESS\tVERSION") // tabwriter reports errors on Flush
	for _, host := range hosts {
		voter := "no"
		if host.Voter {
			voter = "yes"
		}
		_, _ = fmt.Fprintf(table, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
			host.ID, host.Name, host.Phase, voter, host.Underlay, host.PublicAddress, host.Version)
	}
	return table.Flush()
}

func newHost(g *globals) *cobra.Command {
	host := &cobra.Command{Use: "host", Short: "The hosts of the cluster"}
	host.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List the hosts",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := g.connect()
			if err != nil {
				return err
			}
			var hosts []api.HostInfo
			if err := client.Get(cmd.Context(), "/v1/hosts", &hosts); err != nil {
				return err
			}
			return printHosts(cmd.OutOrStdout(), hosts)
		},
	})
	host.AddCommand(newJoinToken(g))
	for _, action := range api.HostActions {
		host.AddCommand(newHostAction(g, action))
	}
	return host
}

// newJoinToken makes the subcommand that makes a join token.
func newJoinToken(g *globals) *cobra.Command {
	var ttl time.Duration
	command := &cobra.Command{
		Use:   "join-token",
		Short: "Make a token one host joins the cluster with (clowderd join --token-file)",
		Long: "Make a token one host joins the cluster with: clowderd join --token-file <file> on the new\n" +
			"host. It opens once and only until it expires; it is shown only now.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := g.connect()
			if err != nil {
				return err
			}
			var answer api.JoinTokenAnswer
			if err := client.Act(cmd.Context(), "/v1/join-tokens", api.JoinTokenRequest{TTL: ttl.String()}, &answer); err != nil {
				return err
			}
			if _, err := fmt.Fprintln(cmd.OutOrStdout(), answer.Token); err != nil {
				return err
			}
			// on stderr, so the token alone goes into a file
			_, err = fmt.Fprintf(cmd.ErrOrStderr(), "valid until %s, for one host\n", answer.Expires.Local().Format(time.RFC3339))
			return err
		},
	}
	command.Flags().DurationVar(&ttl, "ttl", api.DefaultJoinTokenTTL, "how long the token opens: a minute to a day")
	return command
}

// newHostAction makes the subcommand of one action; a final one asks first.
func newHostAction(g *globals, action api.HostAction) *cobra.Command {
	var yes bool
	command := &cobra.Command{
		Use:   action.Name + " <host>",
		Short: action.Description,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := g.connect()
			if err != nil {
				return err
			}
			var hosts []api.HostInfo
			if err := client.Get(cmd.Context(), "/v1/hosts", &hosts); err != nil {
				return err
			}
			host, err := findHost(hosts, args[0])
			if err != nil {
				return err
			}
			if action.Final && !yes {
				if err := confirmHost(cmd, action.Name, host.Name); err != nil {
					return err
				}
			}
			var answer api.HostActionAnswer
			if err := client.Act(cmd.Context(), "/v1/hosts/"+strconv.Itoa(host.ID)+"/"+action.Name, nil, &answer); err != nil {
				return err
			}
			if answer.Leaving {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s: drains, and goes once its stacks and volume copies moved off\n", args[0])
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s: %s done\n", args[0], action.Name)
			return err
		},
	}
	if action.Final {
		command.Flags().BoolVar(&yes, "yes", false, "do it without asking")
	}
	return command
}

// confirmHost asks for the host's name before an action that cannot be
// undone; without a terminal, --yes is the only way. The question goes to
// stderr, so it shows even when the output is redirected.
func confirmHost(cmd *cobra.Command, action, name string) error {
	in, ok := cmd.InOrStdin().(*os.File)
	if !ok || !term.IsTerminal(int(in.Fd())) {
		return fmt.Errorf("host %s cannot be undone: confirm it with --yes", action)
	}
	if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "host %s %s cannot be undone. Type the host's name to go on: ", action, name); err != nil {
		return err
	}
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) { // at EOF the line is what was typed
		return fmt.Errorf("read the confirmation: %w", err)
	}
	if strings.TrimSpace(line) != name {
		return fmt.Errorf("host %s was not confirmed; nothing was sent to the cluster", action)
	}
	return nil
}

// findHost takes a host's name or ID.
func findHost(hosts []api.HostInfo, nameOrID string) (api.HostInfo, error) {
	for _, host := range hosts {
		if host.Name == nameOrID || strconv.Itoa(host.ID) == nameOrID {
			return host, nil
		}
	}
	return api.HostInfo{}, fmt.Errorf("no host %q (clowder host list shows them)", nameOrID)
}
