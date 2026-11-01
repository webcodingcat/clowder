package clusterdir

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/webcodingcat/clowder/internal/sopsage"
)

// SOPSConfigFile tells the sops tool which age keys encrypt the files of a
// directory, so users can edit them with sops.
const SOPSConfigFile = ".sops.yaml"

// NewDirectory is what clowder cluster bootstrap writes into an empty
// cluster directory.
type NewDirectory struct {
	Path string
	// Name is the cluster's name in cluster.yaml.
	Name string
	// Recipient is the user's age key (age1... or an SSH public key); the SOPS
	// files are encrypted for it.
	Recipient    string
	Seed         string
	BootstrapKey string
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

// Create writes the first files of a cluster directory: secrets.sops.yaml
// with the bootstrap key and a placeholder for the hoster token,
// cluster.yaml to fill in and .sops.yaml. It refuses a directory that has a
// cluster.yaml already.
func Create(directory NewDirectory) error {
	if err := checkNoConfig(directory.Path); err != nil {
		return err
	}
	if err := os.MkdirAll(directory.Path, 0o755); err != nil {
		return fmt.Errorf("create the cluster directory: %w", err)
	}
	secrets := Secrets{
		Values:     map[string]string{"hcloud-token": "CHANGE-ME"},
		JoinKeys:   []string{directory.BootstrapKey},
		Registries: map[string]RegistryLogin{},
	}
	if err := WriteSOPS(filepath.Join(directory.Path, SecretsFile), secrets, directory.Recipient); err != nil {
		return err
	}
	if err := WriteSOPSConfig(directory.Path, `\.sops\.yaml$`, directory.Recipient); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(directory.Path, ConfigFile), []byte(configTemplate(directory)), 0o644)
}

// WriteSOPSConfig writes the .sops.yaml of a directory: the files matching
// pathRegex are encrypted for the recipient. Users add their colleagues'
// keys there and run sops updatekeys.
func WriteSOPSConfig(dir, pathRegex, recipient string) error {
	config := fmt.Sprintf("# The sops tool encrypts the files here for these keys; add a colleague's\n"+
		"# key and run sops updatekeys <file> in this directory.\n"+
		"creation_rules:\n  - path_regex: %s\n    age: %s\n", pathRegex, recipient)
	path := filepath.Join(dir, SOPSConfigFile)
	if err := os.WriteFile(path, []byte(config), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// WriteSOPS encrypts a value as YAML for the recipient, as sops would.
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
	lines := []string{
		"# The cluster as a whole; clowder cluster apply sends it, clowder cluster schema",
		"# describes every field. The CLI finds your credentials by the name.",
		"name: " + directory.Name,
		"seeds: [" + directory.Seed + "]",
		"hoster:",
		"  type: hetzner",
		"  token: {secret: hcloud-token}",
		"ingress:",
		"  floating_ip: 203.0.113.10  # CHANGE: the cluster's Floating IP",
		"storage_classes:",
		"  fast: {pool: vg-fast/thin}",
		"  bulk: {pool: vg-bulk/thin}",
		"  any: {fallback: [fast, bulk]}",
		"# the certificates of the HTTP routes come from Let's Encrypt; its notices go here",
		"# acme:",
		"#   email: ops@example.com",
	}
	return strings.Join(lines, "\n") + "\n"
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
