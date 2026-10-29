package cli

import (
	"fmt"
	"io"
	"strconv"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/webcodingcat/clowder/internal/api"
)

func printHosts(out io.Writer, hosts []api.HostInfo) error {
	table := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(table, "ID\tNAME\tPHASE\tVOTER\tUNDERLAY\tVERSION") // tabwriter reports errors on Flush
	for _, host := range hosts {
		voter := "no"
		if host.Voter {
			voter = "yes"
		}
		_, _ = fmt.Fprintf(table, "%d\t%s\t%s\t%s\t%s\t%s\n",
			host.ID, host.Name, host.Phase, voter, host.Underlay, host.Version)
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
	for _, action := range api.HostActions {
		host.AddCommand(&cobra.Command{
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
				id, err := findHost(hosts, args[0])
				if err != nil {
					return err
				}
				if _, err := client.Post(cmd.Context(), "/v1/hosts/"+strconv.Itoa(id)+"/"+action.Name, nil); err != nil {
					return err
				}
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s: %s done\n", args[0], action.Name)
				return err
			},
		})
	}
	return host
}

// findHost takes a host's name or ID.
func findHost(hosts []api.HostInfo, nameOrID string) (int, error) {
	for _, host := range hosts {
		if host.Name == nameOrID || strconv.Itoa(host.ID) == nameOrID {
			return host.ID, nil
		}
	}
	return 0, fmt.Errorf("no host %q (clowder host list shows them)", nameOrID)
}
