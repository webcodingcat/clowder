package stacks

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"github.com/webcodingcat/clowder/internal/atomicfile"
	"github.com/webcodingcat/clowder/internal/compose"
	"github.com/webcodingcat/clowder/internal/digest"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/state"
)

// stackSecrets is what compose up of a stack needs from its secrets.
type stackSecrets struct {
	// files holds the values of each secret that a service asks for as
	// files, by secret and key.
	files map[string]map[string][]byte
	// keys holds the keys of each secret in files, sorted
	// (compose.Target.SecretKeys).
	keys map[string][]string
	// labels holds, for each service that asks for secret files, the
	// digest of those secrets (compose.Target.SecretLabels).
	labels map[string]string
	// env is the second Compose file with the secret variables; it is nil
	// if the stack sets none.
	env []byte
	// digests holds the state's digest of every secret that the stack
	// names, by name. The host reports these as what its containers came
	// up with.
	digests map[string]string
}

// SecretLabelPurpose names the key, derived from the data key, that the
// labels of secret files are made with (Parts.DigestKey).
const SecretLabelPurpose = "clowder secrets label"

// secretLabel is an HMAC-SHA256 over the names and the state's digests of
// one service's secret files, in name order. It changes when any of them
// changes, so Compose recreates the container. It reveals nothing about
// the values to anyone who reads docker inspect.
func secretLabel(key []byte, digests map[string]string, secretNames []string) string {
	var parts [][]byte
	for _, name := range slices.Sorted(slices.Values(secretNames)) {
		parts = append(parts, []byte(name), []byte(digests[name]))
	}
	return digest.HMAC(key, parts...)
}

// openSecrets opens the secrets that a stack names, which are Secret
// resources of its namespace.
func (r *Runner) openSecrets(desired desiredState, stack *state.Stack) (stackSecrets, error) {
	values := map[names.SecretKeyRef]string{}
	secrets := stackSecrets{
		files: map[string]map[string][]byte{}, keys: map[string][]string{},
		labels: map[string]string{}, digests: map[string]string{},
	}
	for _, name := range stack.Spec.SecretNames() {
		secret, ok := desired.secrets[names.ID(stack.Namespace, name)]
		if !ok {
			return stackSecrets{}, fmt.Errorf("stack %s names secret %s, which its namespace does not have", stack.ID(), name)
		}
		for _, key := range secret.Spec.Keys() {
			opened, err := r.open(secret.Spec.Data[key])
			if err != nil {
				return stackSecrets{}, fmt.Errorf("open secret %s of stack %s: %w", name, stack.ID(), err)
			}
			values[names.SecretKeyRef{Name: name, Key: key}] = string(opened)
		}
		secrets.digests[name] = secret.Spec.Digest
	}
	env, err := compose.RenderSecretEnv(stack.Name, stack.Spec, values)
	if err != nil {
		return stackSecrets{}, fmt.Errorf("render the secret variables of stack %s: %w", stack.ID(), err)
	}
	secrets.env = env
	for serviceName, service := range stack.Spec.Services {
		for _, name := range service.Secrets {
			secret := desired.secrets[names.ID(stack.Namespace, name)]
			secrets.keys[name] = secret.Spec.Keys()
			secrets.files[name] = map[string][]byte{}
			for _, key := range secrets.keys[name] {
				secrets.files[name][key] = []byte(values[names.SecretKeyRef{Name: name, Key: key}])
			}
		}
		if len(service.Secrets) > 0 {
			secrets.labels[serviceName] = secretLabel(r.digestKey, secrets.digests, service.Secrets)
		}
	}
	return secrets, nil
}

// writeSecrets writes the new and changed secret files and the secret
// variables, at every pass of a stack that runs here. A file is replaced by
// a rename, so a running container keeps the old value until compose up
// recreates it. An unchanged file stays, and its owner and mode are set
// again; the same holds for the stack's directories. A stack without
// secrets has nothing to protect; removeStaleSecrets empties what it had.
func (r *Runner) writeSecrets(project string, secrets stackSecrets) error {
	if len(secrets.files) == 0 && secrets.env == nil {
		return nil
	}
	// root-only each time, even a directory that already existed with other
	// permissions, because the files below are readable by all. The agent
	// protects SecretsDir itself when it starts (agent.Run).
	for _, dir := range []string{r.files.secretsDir(project), r.files.secretFilesDir(project)} {
		if err := atomicfile.EnsureDir(dir, 0o700); err != nil {
			return err
		}
	}
	for _, name := range slices.Sorted(maps.Keys(secrets.files)) {
		// readable by all, the directory too: dockerd bind-mounts the files
		// as root, and the container reads them as its own user
		if err := atomicfile.EnsureDir(filepath.Join(r.files.secretFilesDir(project), name), 0o755); err != nil {
			return err
		}
		for _, key := range secrets.keys[name] {
			if err := atomicfile.WriteChanged(r.files.secretFile(project, name, key), secrets.files[name][key], 0o444); err != nil {
				return err
			}
		}
	}
	if secrets.env != nil {
		return atomicfile.WriteChanged(r.files.secretEnvFile(project), secrets.env, 0o600)
	}
	return nil
}

// removeStaleSecrets removes the secrets that the stack no longer asks for.
// It runs after compose up, because until then a running container may
// still use them.
func (r *Runner) removeStaleSecrets(project string, secrets stackSecrets) error {
	entries, err := os.ReadDir(r.files.secretFilesDir(project))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, entry := range entries {
		dir := filepath.Join(r.files.secretFilesDir(project), entry.Name())
		values, asked := secrets.files[entry.Name()]
		if !asked || !entry.IsDir() {
			if err := os.RemoveAll(dir); err != nil {
				return err
			}
			continue
		}
		keys, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, key := range keys {
			if _, kept := values[key.Name()]; !kept {
				if err := os.Remove(filepath.Join(dir, key.Name())); err != nil {
					return err
				}
			}
		}
	}
	if secrets.env == nil {
		if err := os.Remove(r.files.secretEnvFile(project)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// removeSecretsOfOthers removes the secrets of the stacks that are not
// placed here and have no Compose file left (compose down ran). These are
// the leavers and whatever a crash left behind.
func (r *Runner) removeSecretsOfOthers(desired desiredState) {
	entries, err := os.ReadDir(r.files.SecretsDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		r.logger.Error("list the stacks' secrets", "error", err)
		return
	}
	for _, entry := range entries {
		project := entry.Name()
		if _, staying := desired.stacks[project]; staying || names.ClowdersOwn(project) {
			continue
		}
		if _, err := os.Stat(r.files.stackFile(project)); !errors.Is(err, os.ErrNotExist) {
			continue // not stopped yet
		}
		if err := os.RemoveAll(r.files.secretsDir(project)); err != nil {
			r.logger.Error("remove the secrets of a stack", "project", project, "error", err)
		}
	}
}
