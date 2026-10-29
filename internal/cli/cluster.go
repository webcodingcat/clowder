package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/webcodingcat/clowder/internal/api"
)

func newCluster(g *globals) *cobra.Command {
	cluster := &cobra.Command{Use: "cluster", Short: "The cluster as a whole"}
	cluster.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "Show the cluster, its leader and its hosts",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := g.connect()
			if err != nil {
				return err
			}
			var status api.Status
			if err := client.Get(cmd.Context(), "/v1/status", &status); err != nil {
				return err
			}
			return printStatus(cmd.OutOrStdout(), status)
		},
	})
	return cluster
}

func printStatus(out io.Writer, status api.Status) error {
	leader := status.Leader
	if leader == "" {
		leader = "none"
	}
	if _, err := fmt.Fprintf(out, "cluster %s, config revision %d, leader %s, index %d\n\n",
		status.Name, status.Revision, leader, status.Index); err != nil {
		return err
	}
	return printHosts(out, status.Hosts)
}
