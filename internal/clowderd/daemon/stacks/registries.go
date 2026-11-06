package stacks

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/webcodingcat/clowder/internal/atomicfile"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/resource"
	"github.com/webcodingcat/clowder/internal/state"
)

// registriesDir holds one Docker config directory per namespace, on the
// secrets' tmpfs. Its config.json has the namespace's registry logins in
// plain text. noLoginsDir holds the prefetcher's config, which has none.
// Both names start with names.ReservedPrefix, so the clean-up of the
// stacks' secrets leaves them alone. RegistryConfigs cleans up registriesDir
// instead.
const (
	registriesDir = names.ReservedPrefix + "registries"
	noLoginsDir   = names.ReservedPrefix + "no-logins"
)

func (f Files) registryConfigDir(namespace string) string {
	return filepath.Join(f.SecretsDir, registriesDir, namespace)
}

// RegistryConfigs keeps the Docker config of each namespace whose stacks
// run on this host. The runner's jobs and the prefetcher share it; its lock
// keeps their writes apart, because atomicfile allows only one writer of a
// path at a time.
type RegistryConfigs struct {
	files      Files
	openSecret OpenSecret
	mu         sync.Mutex
}

// NewRegistryConfigs returns a RegistryConfigs that keeps the configs below
// files.SecretsDir and opens the logins with openSecret.
func NewRegistryConfigs(files Files, openSecret OpenSecret) *RegistryConfigs {
	return &RegistryConfigs{files: files, openSecret: openSecret}
}

// dockerConfig is the part of Docker's config.json that holds logins.
type dockerConfig struct {
	Auths map[string]dockerAuth `json:"auths"`
}

type dockerAuth struct {
	Auth string `json:"auth"` // base64 of "username:password", as docker login writes it
}

// Write writes a namespace's Docker config and returns its directory. Only
// the runner calls it, for a stack that runs here: the control plane opens
// the logins only for such a stack (state.SecretsOfHost). A namespace without
// logins gets an empty config, so that no stack pulls with root's own
// logins. missing names the image pull secrets left out; like the kubelet,
// Write goes on without them. The rename lets a pull read either the old
// logins or the new ones.
func (r *RegistryConfigs) Write(ctx context.Context, current *state.State, namespace string) (
	dir string, missing, err error,
) {
	logins, missing := current.RegistryLogins(namespace)
	config := dockerConfig{Auths: map[string]dockerAuth{}}
	for _, login := range logins {
		values, err := r.openSecret(ctx, login)
		if err != nil {
			return "", nil, fmt.Errorf("open the image pull secret %s of namespace %s: %w", login.Name, namespace, err)
		}
		server := string(values[resource.RegistryServerKey])
		if _, taken := config.Auths[server]; taken {
			missing = errors.Join(missing, fmt.Errorf("namespace %s has a second image pull secret for %s, %s",
				namespace, server, login.Name))
			continue
		}
		credentials := string(values[resource.RegistryUsernameKey]) + ":" + string(values[resource.RegistryPasswordKey])
		config.Auths[server] = dockerAuth{Auth: base64.StdEncoding.EncodeToString([]byte(credentials))}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	dir = r.files.registryConfigDir(namespace)
	if err := writeDockerConfig(dir, config); err != nil {
		return "", nil, fmt.Errorf("write the Docker config of namespace %s: %w", namespace, err)
	}
	return dir, missing, nil
}

// WithoutLogins writes the prefetcher's Docker config, which has no logins,
// and returns its directory. A host gets a namespace's logins only for the
// stacks that run on it, as Kubernetes' node gets a pod's image pull
// secrets only once the pod is bound to it.
func (r *RegistryConfigs) WithoutLogins() (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	dir := filepath.Join(r.files.SecretsDir, noLoginsDir)
	if err := writeDockerConfig(dir, dockerConfig{Auths: map[string]dockerAuth{}}); err != nil {
		return "", fmt.Errorf("write the Docker config without logins: %w", err)
	}
	return dir, nil
}

// writeDockerConfig makes dir and its parent root's alone each time, even
// if they existed with other permissions.
func writeDockerConfig(dir string, config dockerConfig) error {
	encoded, err := json.Marshal(config)
	if err != nil {
		return err
	}
	for _, each := range []string{filepath.Dir(dir), dir} {
		if err := atomicfile.EnsureDir(each, 0o700); err != nil {
			return err
		}
	}
	return atomicfile.WriteChanged(filepath.Join(dir, "config.json"), encoded, 0o600)
}

func (r *Runner) removeRegistryConfigsOfOthers(desired desiredState) {
	keep := map[string]bool{}
	for _, stack := range desired.stacks {
		keep[stack.Namespace] = true
	}
	if err := r.registries.RemoveOthers(keep); err != nil {
		r.logger.Error("remove the registry logins of namespaces without stacks here", "error", err)
	}
}

// RemoveOthers removes the Docker configs of the namespaces that are not in
// keep, so that their logins stay on the host only while one of their
// stacks runs here.
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
