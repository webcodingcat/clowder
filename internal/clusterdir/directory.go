// Package clusterdir reads a cluster directory: cluster.yaml, the shared
// secrets in secrets.sops.yaml and each user's credentials under
// credentials/. It is portable (Linux, macOS, Windows); the SOPS files are
// decrypted with the user's age key as SOPS finds it (SOPS_AGE_KEY_FILE,
// SOPS_AGE_KEY or SOPS's default path).
package clusterdir

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/getsops/sops/v3/decrypt"
	"go.yaml.in/yaml/v3"
)

// File names inside a cluster directory.
const (
	ConfigFile      = "cluster.yaml"
	SecretsFile     = "secrets.sops.yaml"
	CredentialsDir  = "credentials"
	credentialsTail = ".sops.yaml"
	// EnvVar names the cluster directory when --cluster is not given.
	EnvVar = "CLOWDER_CLUSTER"
)

// Directory is a loaded cluster directory.
type Directory struct {
	Path   string
	Config Config
}

// Secrets is secrets.sops.yaml after decryption.
type Secrets struct {
	// Values are referenced from cluster.yaml as {secret: name}.
	Values map[string]string `yaml:"secrets"`
	// JoinKeys are the bootstrap keys hosts may join with.
	JoinKeys []string `yaml:"join_keys"`
	// Registries hold the credentials for private image registries.
	Registries map[string]RegistryLogin `yaml:"registries"`
}

// RegistryLogin is the login for one registry.
type RegistryLogin struct {
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// Credentials is one user's credentials/<user>.sops.yaml after decryption.
type Credentials struct {
	User        string `yaml:"user"`
	CA          string `yaml:"ca"`
	Certificate string `yaml:"certificate"`
	Key         string `yaml:"key"`
}

// Find returns the cluster directory: the flag if set, else $CLOWDER_CLUSTER,
// else the working directory if it holds a cluster.yaml.
func Find(flag string) (string, error) {
	for _, candidate := range []string{flag, os.Getenv(EnvVar)} {
		if candidate != "" {
			return candidate, nil
		}
	}
	workingDir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("find the cluster directory: %w", err)
	}
	if _, err := os.Stat(filepath.Join(workingDir, ConfigFile)); err == nil {
		return workingDir, nil
	}
	return "", fmt.Errorf("no cluster directory: use --cluster, set %s or run where %s is",
		EnvVar, ConfigFile)
}

// Load reads and checks cluster.yaml in dir.
func Load(dir string) (*Directory, error) {
	content, err := os.ReadFile(filepath.Join(dir, ConfigFile))
	if err != nil {
		return nil, fmt.Errorf("read the cluster directory: %w", err)
	}
	var config Config
	if err := decodeStrict(content, &config); err != nil {
		return nil, fmt.Errorf("%s: %w", ConfigFile, err)
	}
	if problems := Check(config); len(problems) > 0 {
		return nil, problems
	}
	return &Directory{Path: dir, Config: config}, nil
}

// Secrets decrypts secrets.sops.yaml and checks that every secret that
// cluster.yaml names is there.
func (d *Directory) Secrets() (*Secrets, error) {
	var secrets Secrets
	if err := d.decryptInto(filepath.Join(d.Path, SecretsFile), &secrets); err != nil {
		return nil, err
	}
	var missing Problems
	for _, name := range SecretNames(d.Config) {
		if _, ok := secrets.Values[name]; !ok {
			missing = append(missing, fmt.Sprintf("secret %q is used in %s but missing in %s",
				name, ConfigFile, SecretsFile))
		}
	}
	if len(missing) > 0 {
		return nil, missing
	}
	return &secrets, nil
}

// Credentials decrypts the credentials of one user.
func (d *Directory) Credentials(user string) (*Credentials, error) {
	var credentials Credentials
	file := filepath.Join(d.Path, CredentialsDir, user+credentialsTail)
	if err := d.decryptInto(file, &credentials); err != nil {
		return nil, err
	}
	if credentials.User != user {
		return nil, fmt.Errorf("%s belongs to %q, not to %q", file, credentials.User, user)
	}
	return &credentials, nil
}

// decryptInto decrypts a SOPS file and decodes it strictly into target.
func (d *Directory) decryptInto(file string, target any) error {
	cleartext, err := decrypt.File(file, "yaml")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%s is missing: %w", file, err)
		}
		return fmt.Errorf("decrypt %s (is your age key set up for SOPS?): %w", file, err)
	}
	if err := decodeStrict(cleartext, target); err != nil {
		return fmt.Errorf("%s: %w", file, err)
	}
	return nil
}

// decodeStrict decodes YAML and refuses keys the target does not know, so a
// misspelled key is reported instead of silently ignored.
func decodeStrict(content []byte, target any) error {
	decoder := yaml.NewDecoder(bytes.NewReader(content))
	decoder.KnownFields(true)
	return decoder.Decode(target)
}
