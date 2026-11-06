package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/webcodingcat/clowder/internal/api"
)

func newCluster(g *globals) *cobra.Command {
	cluster := &cobra.Command{Use: "cluster", Short: "The cluster as a whole"}
	cluster.AddCommand(newStatus(g))
	return cluster
}

func newStatus(g *globals) *cobra.Command {
	return &cobra.Command{
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
			if err := printStatus(cmd.OutOrStdout(), status); err != nil {
				return err
			}
			// the leader keeps the failed deliveries; without a leader
			// they are unknown, but the rest of the status still shows
			var failures []api.AlertFailure
			failuresErr := client.Get(cmd.Context(), "/v1/alerts/failures", &failures)
			return printAlerts(cmd.OutOrStdout(), status.Alerts, failures, failuresErr)
		},
	}
}

func printStatus(out io.Writer, status api.Status) error {
	leader := status.Leader
	if leader == "" {
		leader = "none"
	}
	configuration := fmt.Sprintf("configuration generation %d", status.Generation)
	if status.Generation == 0 {
		configuration = "no configuration"
	}
	if _, err := fmt.Fprintf(out, "cluster %s, %s, leader %s, index %d\n\n",
		status.Name, configuration, leader, status.Index); err != nil {
		return err
	}
	return printHosts(out, status.Hosts)
}
