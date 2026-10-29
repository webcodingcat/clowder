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
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/webcodingcat/clowder/internal/pki"
	"github.com/webcodingcat/clowder/internal/sopsage"
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
	Username string `yaml:"username" json:"username"`
	Password string `yaml:"password" json:"password"`
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
	dir, err := Target(flag)
	if err != nil || flag != "" || os.Getenv(EnvVar) != "" {
		return dir, err
	}
	if _, err := os.Stat(filepath.Join(dir, ConfigFile)); err != nil {
		return "", fmt.Errorf("no cluster directory: use --cluster, set %s or run where %s is",
			EnvVar, ConfigFile)
	}
	return dir, nil
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
	config = config.WithDefaults()
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

// UserEnvVar names the user when --user is not given.
const UserEnvVar = "CLOWDER_USER"

// User returns who uses the CLI: the flag if set, else $CLOWDER_USER, else
// the only user with credentials in the directory.
func (d *Directory) User(flag string) (string, error) {
	for _, candidate := range []string{flag, os.Getenv(UserEnvVar)} {
		if candidate != "" {
			return candidate, nil
		}
	}
	files, err := filepath.Glob(filepath.Join(d.Path, CredentialsDir, "*"+credentialsTail))
	if err != nil {
		return "", fmt.Errorf("list the credentials: %w", err)
	}
	switch len(files) {
	case 0:
		return "", fmt.Errorf("no credentials in %s", filepath.Join(d.Path, CredentialsDir))
	case 1:
		return strings.TrimSuffix(filepath.Base(files[0]), credentialsTail), nil
	default:
		return "", fmt.Errorf("%d users have credentials here: use --user or set %s", len(files), UserEnvVar)
	}
}

// Credentials decrypts the credentials of one user.
func (d *Directory) Credentials(user string) (*Credentials, error) {
	if err := pki.CheckUserName(user); err != nil {
		return nil, err
	}
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
	cleartext, err := ReadSOPS(file)
	if err != nil {
		return err
	}
	if err := decodeStrict(cleartext, target); err != nil {
		return fmt.Errorf("%s: %w", file, err)
	}
	return nil
}

// ReadSOPS decrypts a SOPS file with the user's age key, as sops finds it.
// It refuses a file with values left in clear (a rule like
// unencrypted_suffix): these files hold secrets, and such a value would sit
// in the repository in clear.
func ReadSOPS(file string) ([]byte, error) {
	sealed, err := os.ReadFile(file)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%s is missing: %w", file, err)
		}
		return nil, fmt.Errorf("read %s: %w", file, err)
	}
	identities, err := sopsage.LoadIdentities()
	if err != nil {
		return nil, fmt.Errorf("load the age key for %s: %w", file, err)
	}
	cleartext, err := sopsage.Decrypt(sealed, identities)
	if err != nil {
		return nil, fmt.Errorf("decrypt %s (is your age key set up for SOPS?): %w", file, err)
	}
	inClear, err := sopsage.LeftInClear(sealed)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", file, err)
	}
	if len(inClear) > 0 {
		return nil, fmt.Errorf("%s keeps %s in clear; encrypt every value (no unencrypted_suffix)",
			file, strings.Join(inClear, ", "))
	}
	return cleartext, nil
}

// decodeStrict decodes YAML and refuses keys the target does not know, so a
// misspelled key is reported instead of silently ignored.
func decodeStrict(content []byte, target any) error {
	decoder := yaml.NewDecoder(bytes.NewReader(content))
	decoder.KnownFields(true)
	return decoder.Decode(target)
}
