package clusterdir

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/webcodingcat/clowder/internal/secretvalue"
	"github.com/webcodingcat/clowder/internal/sopsage"
)

// NewDirectory is what clowder cluster bootstrap writes into an empty
// cluster directory.
type NewDirectory struct {
	Path string
	// Name is the cluster's name in cluster.yaml.
	Name string
	Seed string
	// JoinKey is the bootstrap key, encrypted for cluster.yaml
	// (secretvalue's armor).
	JoinKey string
}

// CheckBootstrap tells whether a bootstrap may write a cluster of that name
// into path: not over a cluster.yaml, and not over credentials of a cluster
// of the same name unless force says so (a cluster set up anew). The CLI
// asks before it contacts the cluster, so nothing fails after the cluster
// has its first user.
func CheckBootstrap(path, cluster string, force bool) error {
	if err := checkNoConfig(path); err != nil {
		return err
	}
	credentials, err := CredentialsPath(cluster)
	if err != nil {
		return err
	}
	_, err = os.Stat(credentials)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("look for credentials: %w", err)
	case !force:
		return fmt.Errorf("you have credentials for a cluster %s already (%s): "+
			"give the new cluster another name, or replace them with --force", cluster, credentials)
	}
	return nil
}

// checkNoConfig refuses a directory that has a cluster.yaml.
func checkNoConfig(path string) error {
	if _, err := os.Stat(filepath.Join(path, ConfigFile)); err == nil {
		return fmt.Errorf("%s has a %s already", path, ConfigFile)
	}
	return nil
}

// Create writes cluster.yaml into a new cluster directory: the name, the
// seed and the bootstrap key, and as comments what the first cluster apply
// still needs. It refuses a directory that has a cluster.yaml already.
func Create(directory NewDirectory) error {
	if err := checkNoConfig(directory.Path); err != nil {
		return err
	}
	if err := os.MkdirAll(directory.Path, 0o755); err != nil {
		return fmt.Errorf("create the cluster directory: %w", err)
	}
	return os.WriteFile(filepath.Join(directory.Path, ConfigFile), []byte(configTemplate(directory)), 0o644)
}

// WriteSOPS encrypts a value as YAML for the recipient, as sops would; for
// the SOPS files of Secret resources.
func WriteSOPS(path string, value any, recipient string) error {
	plain, err := yaml.Marshal(value)
	if err != nil {
		return err
	}
	sealed, err := sopsage.Encrypt(plain, []string{recipient}, time.Now())
	if err != nil {
		return fmt.Errorf("encrypt %s: %w", filepath.Base(path), err)
	}
	if err := os.WriteFile(path, sealed, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// configTemplate is cluster.yaml for the user to finish.
func configTemplate(directory NewDirectory) string {
	return strings.Join([]string{
		"# The cluster as a whole; clowder cluster apply sends it, clowder cluster schema",
		"# describes every field. The CLI finds your credentials by the name. A secret",
		"# stands here encrypted, tagged !secret: clowder secret encrypt --cluster-yaml",
		"# writes one, and only the cluster opens it.",
		"name: " + directory.Name,
		"seeds: [" + directory.Seed + "]",
		"# the bootstrap keys: hosts join with them",
		"join_keys:",
		"  - " + secretvalue.Block(directory.JoinKey, "    ") +
			"# Before the first clowder cluster apply, fill in the rest (clowder cluster",
		"# schema has every field), for example:",
		"# hoster:",
		"#   type: hetzner",
		"#   token: !secret |",
		"#     <clowder secret encrypt --cluster-yaml, given the hoster's API token>",
		"# ingress:",
		"#   floating_ip: 203.0.113.10",
		"# storage_classes:",
		"#   fast: {pool: vg-fast/thin}",
		"#   bulk: {pool: vg-bulk/thin}",
		"#   any: {fallback: [fast, bulk]}",
		"# The certificates of the HTTP routes come from Let's Encrypt; its notices go to:",
		"# acme:",
		"#   email: ops@example.com",
	}, "\n") + "\n"
}

// Target is the directory a new cluster goes into: the flag, else
// $CLOWDER_CLUSTER, else the working directory. Unlike Find, it does not ask
// for a cluster.yaml.
func Target(flag string) (string, error) {
	for _, candidate := range []string{flag, os.Getenv(EnvVar)} {
		if candidate != "" {
			return candidate, nil
		}
	}
	workingDir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("find the cluster directory: %w", err)
	}
	return workingDir, nil
}
