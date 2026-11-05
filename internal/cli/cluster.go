package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/webcodingcat/clowder/internal/api"
	"github.com/webcodingcat/clowder/internal/clusterdir"
	"github.com/webcodingcat/clowder/internal/clusterspec"
)

func newCluster(g *globals) *cobra.Command {
	cluster := &cobra.Command{Use: "cluster", Short: "The cluster as a whole"}
	cluster.AddCommand(newStatus(g), newBootstrap(g), newApply(g), newDiff(g), newSchema())
	return cluster
}

func newStatus(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the cluster, its leader and its hosts",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, _, err := g.connect()
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
			// the leader keeps the failed deliveries; without a leader they
			// are unknown, and the rest of the status still shows
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
	if _, err := fmt.Fprintf(out, "cluster %s, config revision %d, leader %s, index %d\n\n",
		status.Name, status.Revision, leader, status.Index); err != nil {
		return err
	}
	return printHosts(out, status.Hosts)
}

// localApply is what clowder cluster apply sends: cluster.yaml, checked
// here first so its problems show without a round trip, its values still
// encrypted, and its files. Its warnings go to warnings.
func localApply(directory *clusterdir.Directory, warnings io.Writer) (api.ClusterApply, error) {
	if problems := clusterspec.Check(directory.Config); len(problems) > 0 {
		return api.ClusterApply{}, problems
	}
	for _, warning := range clusterspec.EndpointWarnings(directory.Config) {
		if _, err := fmt.Fprintf(warnings, "warning: %s\n", warning); err != nil {
			return api.ClusterApply{}, err
		}
	}
	files, err := directory.Files()
	if err != nil {
		return api.ClusterApply{}, err
	}
	return api.ClusterApply{Spec: directory.Config, Encrypted: directory.Encrypted, Files: files}, nil
}

func newApply(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "apply",
		Short: "Send cluster.yaml to the cluster",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, directory, err := g.connect()
			if err != nil {
				return err
			}
			apply, err := localApply(directory, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			if _, err := client.Put(cmd.Context(), "/v1/cluster", apply); err != nil {
				return err
			}
			var applied api.ClusterInfo
			if err := client.Get(cmd.Context(), "/v1/cluster", &applied); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "applied: revision %d\n", applied.Revision)
			return err
		},
	}
}

func newDiff(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "diff",
		Short: "Show what cluster apply would change",
		Long: "Asks the cluster to try cluster.yaml without applying it: a problem shows as an apply\n" +
			"would show it, else the fields, files and secrets that would change. A secret encrypted\n" +
			"anew with the same value is no change.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, directory, err := g.connect()
			if err != nil {
				return err
			}
			apply, err := localApply(directory, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			diff, err := client.DiffCluster(cmd.Context(), apply)
			if err != nil {
				return err
			}
			return printDiff(cmd.OutOrStdout(), diff)
		},
	}
}

// printDiff prints what a cluster apply would change.
func printDiff(out io.Writer, diff api.ClusterDiff) error {
	lines := []string{}
	if diff.Revision == 0 {
		lines = append(lines, "cluster.yaml was never applied")
	}
	if len(diff.Fields) > 0 {
		lines = append(lines, fmt.Sprintf("cluster.yaml differs in: %v", diff.Fields))
	}
	for _, name := range diff.Files {
		lines = append(lines, "the file "+name+" differs")
	}
	for _, name := range diff.Secrets {
		lines = append(lines, "the secret "+name+" differs")
	}
	if len(lines) == 0 {
		lines = append(lines, fmt.Sprintf("nothing to apply (revision %d)", diff.Revision))
	}
	for _, line := range lines {
		if _, err := fmt.Fprintln(out, line); err != nil {
			return err
		}
	}
	return nil
}

func newSchema() *cobra.Command {
	return &cobra.Command{
		Use:   "schema",
		Short: "Print the JSON schema of cluster.yaml (for editors)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			schema, err := clusterdir.Schema()
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), string(schema))
			return err
		},
	}
}
