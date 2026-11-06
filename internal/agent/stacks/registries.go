package stacks

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/webcodingcat/clowder/internal/atomicfile"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/state"
)

// registriesDir holds one Docker config directory per namespace, on the
// secrets' tmpfs. Its config.json has the namespace's registry logins in
// plain text. The name starts with names.ReservedPrefix, so the clean-up of
// the stacks' secrets leaves it alone. RegistryConfigs cleans it up instead.
const registriesDir = names.ReservedPrefix + "registries"

// registryConfigDir is the Docker config directory with which a namespace's
// images are pulled (docker --config).
func (f Files) registryConfigDir(namespace string) string {
	return filepath.Join(f.SecretsDir, registriesDir, namespace)
}

// RegistryConfigs writes the Docker config of each namespace whose images
// this host pulls, and removes the configs of the other namespaces. The
// runner's jobs and the prefetcher share one RegistryConfigs and write from
// separate goroutines. Its lock keeps the writers apart, because atomicfile
// allows only one writer of a path at a time.
type RegistryConfigs struct {
	files Files
	open  func(state.Encrypted) ([]byte, error)
	mu    sync.Mutex
}

// NewRegistryConfigs returns a RegistryConfigs that keeps the configs below
// files.SecretsDir and decrypts the logins with open.
func NewRegistryConfigs(files Files, open func(state.Encrypted) ([]byte, error)) *RegistryConfigs {
	return &RegistryConfigs{files: files, open: open}
}

// dockerConfig is the part of Docker's config.json that holds logins.
type dockerConfig struct {
	Auths map[string]dockerAuth `json:"auths"`
}

type dockerAuth struct {
	Auth string `json:"auth"` // base64 of "username:password", as docker login writes it
}

// Write decrypts a namespace's registry logins (state.RegistryLogins),
// writes them into the namespace's Docker config and returns the config's
// directory. A namespace without logins gets an empty config, so that no
// stack pulls with the logins of root's own config. missing names the image
// pull secrets that Write left out. Like the kubelet, Write goes on without
// them, and a pull that fails says why. The file is replaced by a rename,
// so a pull reads either the old logins or the new ones.
func (r *RegistryConfigs) Write(current *state.State, namespace string) (dir string, missing, err error) {
	logins, missing := current.RegistryLogins(namespace)
	config := dockerConfig{Auths: map[string]dockerAuth{}}
	for _, login := range logins {
		var parts [3][]byte
		for index, encrypted := range []state.Encrypted{login.Server, login.Username, login.Password} {
			if parts[index], err = r.open(encrypted); err != nil {
				return "", nil, fmt.Errorf("open the image pull secret %s of namespace %s: %w", login.Secret, namespace, err)
			}
		}
		server := string(parts[0])
		if _, taken := config.Auths[server]; taken {
			missing = errors.Join(missing, fmt.Errorf("namespace %s has a second image pull secret for %s, %s",
				namespace, server, login.Secret))
			continue
		}
		config.Auths[server] = dockerAuth{
			Auth: base64.StdEncoding.EncodeToString([]byte(string(parts[1]) + ":" + string(parts[2]))),
		}
	}
	encoded, err := json.Marshal(config)
	if err != nil {
		return "", nil, fmt.Errorf("encode the Docker config of namespace %s: %w", namespace, err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	dir = r.files.registryConfigDir(namespace)
	for _, each := range []string{filepath.Dir(dir), dir} {
		if err := atomicfile.EnsureDir(each, 0o700); err != nil {
			return "", nil, err
		}
	}
	if err := atomicfile.WriteChanged(filepath.Join(dir, "config.json"), encoded, 0o600); err != nil {
		return "", nil, err
	}
	return dir, missing, nil
}

// removeRegistryConfigsOfOthers keeps the Docker configs of the namespaces
// that have a stack which runs here or which this host may take (the
// prefetcher's stacks), and removes the others.
func (r *Runner) removeRegistryConfigsOfOthers(desired desiredState) {
	keep := map[string]bool{}
	for _, stack := range desired.stacks {
		keep[stack.Namespace] = true
	}
	for _, stack := range desired.copied.Stacks {
		if mayTake(desired.copied, stack, r.host) {
			keep[stack.Namespace] = true
		}
	}
	if err := r.registries.RemoveOthers(keep); err != nil {
		r.logger.Error("remove the registry logins of namespaces without stacks here", "error", err)
	}
}

// RemoveOthers removes the Docker configs of the namespaces that are not in
// keep, so that their logins stay on the host only as long as one of their
// stacks may run here.
func (r *RegistryConfigs) RemoveOthers(keep map[string]bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	parent := filepath.Join(r.files.SecretsDir, registriesDir)
	entries, err := os.ReadDir(parent)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, entry := range entries {
		if !keep[entry.Name()] {
			if err := os.RemoveAll(filepath.Join(parent, entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}
