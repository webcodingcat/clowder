package sopsage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"filippo.io/age"
	"filippo.io/age/agessh"
)

// LoadIdentities collects the user's age identities where SOPS looks for
// them: an SSH key (SOPS_AGE_SSH_PRIVATE_KEY_FILE, else ~/.ssh/id_ed25519 and
// ~/.ssh/id_rsa), the keys in SOPS_AGE_KEY, and the key file
// (SOPS_AGE_KEY_FILE, else sops/age/keys.txt in the user's config
// directory). SOPS_AGE_KEY_CMD and SSH keys with a passphrase are not
// supported. Missing default files are skipped; a file named in a variable
// must exist.
func LoadIdentities() ([]age.Identity, error) {
	var identities []age.Identity
	sshIdentities, err := loadSSHIdentities()
	if err != nil {
		return nil, err
	}
	identities = append(identities, sshIdentities...)
	if keys := os.Getenv("SOPS_AGE_KEY"); keys != "" {
		parsed, err := age.ParseIdentities(strings.NewReader(keys))
		if err != nil {
			return nil, fmt.Errorf("read SOPS_AGE_KEY: %w", err)
		}
		identities = append(identities, parsed...)
	}
	keyFile, named := os.LookupEnv("SOPS_AGE_KEY_FILE")
	if !named || keyFile == "" {
		configDir, err := userConfigDir()
		if err != nil {
			return identities, nil //nolint:nilerr // no config directory, so no default key file
		}
		keyFile, named = filepath.Join(configDir, "sops", "age", "keys.txt"), false
	}
	content, err := os.ReadFile(keyFile)
	switch {
	case errors.Is(err, os.ErrNotExist) && !named:
		return identities, nil
	case err != nil:
		return nil, fmt.Errorf("read the age key file: %w", err)
	}
	parsed, err := age.ParseIdentities(strings.NewReader(string(content)))
	if err != nil {
		return nil, fmt.Errorf("read the age key file %s: %w", keyFile, err)
	}
	return append(identities, parsed...), nil
}

// loadSSHIdentities reads the SSH key SOPS would use. A default key that
// age cannot use (one with a passphrase, say) is skipped, as the age key
// file may still open the file.
func loadSSHIdentities() ([]age.Identity, error) {
	if keyFile := os.Getenv("SOPS_AGE_SSH_PRIVATE_KEY_FILE"); keyFile != "" {
		identity, err := readSSHIdentity(keyFile)
		if err != nil {
			return nil, err
		}
		return []age.Identity{identity}, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, nil //nolint:nilerr // no home directory, so no default SSH key
	}
	var identities []age.Identity
	for _, name := range []string{"id_ed25519", "id_rsa"} {
		if identity, err := readSSHIdentity(filepath.Join(home, ".ssh", name)); err == nil {
			identities = append(identities, identity)
		}
	}
	return identities, nil
}

// readSSHIdentity reads an unencrypted SSH private key.
func readSSHIdentity(keyFile string) (age.Identity, error) {
	content, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, fmt.Errorf("read the SSH key: %w", err)
	}
	identity, err := agessh.ParseIdentity(content)
	if err != nil {
		return nil, fmt.Errorf("read the SSH key %s: %w", keyFile, err)
	}
	return identity, nil
}

// userConfigDir is the directory SOPS takes its default key file from; on
// macOS, SOPS prefers XDG_CONFIG_HOME over Go's default.
func userConfigDir() (string, error) {
	if runtime.GOOS == "darwin" {
		if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
			return dir, nil
		}
	}
	return os.UserConfigDir()
}
