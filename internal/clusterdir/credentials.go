package clusterdir

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"go.yaml.in/yaml/v3"

	"github.com/webcodingcat/clowder/internal/atomicfile"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/pki"
)

// Credentials are the user's way into one cluster: the user's name, key and
// certificate, and the cluster's CA, which is what the CLI trusts. They live
// in the user's config directory, not in the cluster directory: a
// repository is shared, the key is the user's alone.
type Credentials struct {
	User        string `yaml:"user"`
	CA          string `yaml:"ca"`
	Certificate string `yaml:"certificate"`
	Key         string `yaml:"key"`
}

// CredentialsPath is where the credentials for the cluster of that name
// live: <user config directory>/clowder/clusters/<cluster>/credentials.yaml.
// The config directory is os.UserConfigDir's: $XDG_CONFIG_HOME or ~/.config
// on Linux, ~/Library/Application Support on macOS, %AppData% on Windows.
func CredentialsPath(cluster string) (string, error) {
	// the name becomes a directory; clusterspec.Check holds cluster.yaml's
	// name to the same rule
	if !names.Valid(cluster) {
		return "", fmt.Errorf("cluster name %q: %s", cluster, names.Rule)
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("find the credentials: %w", err)
	}
	return filepath.Join(configDir, "clowder", "clusters", cluster, "credentials.yaml"), nil
}

// ReadCredentials reads the credentials for the cluster of that name.
func ReadCredentials(cluster string) (*Credentials, error) {
	path, err := CredentialsPath(cluster)
	if err != nil {
		return nil, err
	}
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("no credentials for cluster %s (%s): you are not a user of it on this machine",
			cluster, path)
	}
	if err != nil {
		return nil, fmt.Errorf("read the credentials: %w", err)
	}
	var credentials Credentials
	if err := decodeStrict(content, &credentials); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := pki.CheckUserName(credentials.User); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &credentials, nil
}

// WriteCredentials writes the credentials for the cluster of that name,
// readable by the user alone (0600 in a directory of 0700, as SSH keeps its
// keys; on Windows the user's profile protects them), replacing any there.
func WriteCredentials(cluster string, credentials Credentials) error {
	path, err := CredentialsPath(cluster)
	if err != nil {
		return err
	}
	content, err := yaml.Marshal(credentials)
	if err != nil {
		return err
	}
	return atomicfile.Write(path, content, 0o600)
}
