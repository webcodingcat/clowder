package cli

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"

	"github.com/spf13/cobra"

	"github.com/webcodingcat/clowder/internal/api"
	"github.com/webcodingcat/clowder/internal/buildinfo"
	"github.com/webcodingcat/clowder/internal/clusterdir"
	"github.com/webcodingcat/clowder/internal/clusterspec"
	"github.com/webcodingcat/clowder/internal/pki"
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

func newBootstrap(g *globals) *cobra.Command {
	var recipient, seed, keyFile string
	command := &cobra.Command{
		Use:   "bootstrap <user>",
		Short: "Become the first user of a new cluster and set up its directory",
		Long: "Asks the first host for the cluster's CA, trusts it only if the host proves it knows the\n" +
			"bootstrap key, and gets the first user's certificate. Writes the cluster directory:\n" +
			"credentials/<user>.sops.yaml, secrets.sops.yaml, cluster.yaml to fill in and .sops.yaml.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			user := args[0]
			if err := pki.CheckUserName(user); err != nil {
				return err
			}
			path, err := clusterdir.Target(g.cluster)
			if err != nil {
				return err
			}
			bootstrapKey, err := pki.ReadBootstrapKey(keyFile)
			if err != nil {
				return err
			}
			key, err := pki.NewKey()
			if err != nil {
				return err
			}
			caPEM, certificatePEM, err := api.Bootstrap(cmd.Context(), seed, []byte(bootstrapKey), user,
				key.Public().(ed25519.PublicKey), buildinfo.Version)
			if err != nil {
				return err
			}
			keyPEM, err := pki.EncodeKey(key)
			if err != nil {
				return err
			}
			err = clusterdir.Create(clusterdir.NewDirectory{
				Path: path, Recipient: recipient, Seed: seed, BootstrapKey: bootstrapKey,
				Credentials: clusterdir.Credentials{User: user, CA: caPEM, Certificate: certificatePEM, Key: string(keyPEM)},
			})
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s is the first user of the cluster. Next:\n"+
				"  1. fill in cluster.yaml (the Floating IP, the storage classes)\n"+
				"  2. sops edit secrets.sops.yaml (the hoster token)\n"+
				"  3. clowder cluster apply\n", user)
			return err
		},
	}
	command.Flags().StringVar(&recipient, "age", "", "your age public key (age1...) or SSH public key")
	command.Flags().StringVar(&seed, "seed", "", "address of the first host")
	command.Flags().StringVar(&keyFile, "key-file", "",
		"a file with the cluster's bootstrap key (from OpenTofu); not the key itself, which shell histories would keep")
	for _, name := range []string{"age", "seed", "key-file"} {
		_ = command.MarkFlagRequired(name) // the flags exist; this cannot fail
	}
	return command
}

// localApply is what clowder cluster apply would send.
func localApply(directory *clusterdir.Directory) (api.ClusterApply, error) {
	secrets, err := directory.Secrets()
	if err != nil {
		return api.ClusterApply{}, err
	}
	files, err := directory.Files()
	if err != nil {
		return api.ClusterApply{}, err
	}
	return api.ClusterApply{
		Spec: directory.Config, Secrets: secrets.Values, JoinKeys: secrets.JoinKeys, Registries: secrets.Registries,
		Files: files,
	}, nil
}

func newApply(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "apply",
		Short: "Send cluster.yaml and the secrets to the cluster",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, directory, err := g.connect()
			if err != nil {
				return err
			}
			apply, err := localApply(directory)
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
		Short: "Show whether cluster.yaml and the secrets differ from what the cluster runs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, directory, err := g.connect()
			if err != nil {
				return err
			}
			local, err := localApply(directory)
			if err != nil {
				return err
			}
			hash, err := local.Hash()
			if err != nil {
				return err
			}
			var applied api.ClusterInfo
			if err := client.Get(cmd.Context(), "/v1/cluster", &applied); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if hash == applied.Hash {
				_, err = fmt.Fprintf(out, "nothing to apply (revision %d)\n", applied.Revision)
				return err
			}
			changed, err := changedFields(applied.Spec, local.Spec)
			if err != nil {
				return err
			}
			if len(changed) > 0 {
				if _, err := fmt.Fprintf(out, "cluster.yaml differs in: %v\n", changed); err != nil {
					return err
				}
			}
			for _, name := range changedFiles(applied.Files, local.Files) {
				if _, err := fmt.Fprintf(out, "the file %s differs\n", name); err != nil {
					return err
				}
			}
			// the applied cluster.yaml and files with the local secrets: if
			// that hash differs too, the secrets changed
			secretsOnly := local
			secretsOnly.Spec, secretsOnly.Files = applied.Spec, applied.Files
			secretsHash, err := secretsOnly.Hash()
			if err != nil || secretsHash == applied.Hash {
				return err
			}
			_, err = fmt.Fprintln(out, "the secrets differ (secrets.sops.yaml)")
			return err
		},
	}
}

// changedFields lists the top-level keys of cluster.yaml whose values
// differ.
func changedFields(applied, local clusterspec.Config) ([]string, error) {
	fields := func(config clusterspec.Config) (map[string]json.RawMessage, error) {
		encoded, err := json.Marshal(config)
		if err != nil {
			return nil, err
		}
		var byField map[string]json.RawMessage
		return byField, json.Unmarshal(encoded, &byField)
	}
	before, err := fields(applied)
	if err != nil {
		return nil, err
	}
	after, err := fields(local)
	if err != nil {
		return nil, err
	}
	var changed []string
	for _, name := range slices.Sorted(maps.Keys(after)) {
		if !bytes.Equal(before[name], after[name]) {
			changed = append(changed, name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(before)) {
		if _, kept := after[name]; !kept {
			changed = append(changed, name)
		}
	}
	return changed, nil
}

// changedFiles lists the files next to cluster.yaml that came, went or
// changed.
func changedFiles(applied, local map[string][]byte) []string {
	var changed []string
	for name, content := range local {
		if applies, ok := applied[name]; !ok || !bytes.Equal(applies, content) {
			changed = append(changed, name)
		}
	}
	for name := range applied {
		if _, kept := local[name]; !kept {
			changed = append(changed, name)
		}
	}
	slices.Sort(changed)
	return changed
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
