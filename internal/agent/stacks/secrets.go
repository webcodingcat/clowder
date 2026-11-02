package stacks

import (
	"bytes"
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

// stackSecrets are what compose up of a stack needs of its secrets.
type stackSecrets struct {
	// files holds the value of each secret a service asks for as a file.
	files map[string][]byte
	// labels holds, per service that asks for secret files, the digest of
	// those secrets (compose.Target.SecretLabels).
	labels map[string]string
	// env is the second Compose file with the secret variables; nil if the
	// stack sets none.
	env []byte
	// digests are those of every secret the stack names, as the state has
	// them, by name: what the host reports its containers came up with.
	digests map[string]string
}

// SecretLabelPurpose names the key, derived from the data key, that the
// labels of secret files are made with (Parts.DigestKey).
const SecretLabelPurpose = "clowder secrets label"

// secretLabel is an HMAC-SHA256 over the names and the state's digests of
// the secret files of one service, in name order: it changes with any of
// them, so Compose recreates the container, and says nothing about the
// values to whoever reads docker inspect.
func secretLabel(key []byte, digests map[string]string, secretNames []string) string {
	var parts [][]byte
	for _, name := range slices.Sorted(slices.Values(secretNames)) {
		parts = append(parts, []byte(name), []byte(digests[name]))
	}
	return digest.HMAC(key, parts...)
}

// openSecrets opens the secrets a stack names, the Secret resources of its
// namespace.
func (r *Runner) openSecrets(desired desiredState, stack *state.Stack) (stackSecrets, error) {
	values := map[string]string{}
	secrets := stackSecrets{files: map[string][]byte{}, labels: map[string]string{}, digests: map[string]string{}}
	for _, name := range stack.Spec.SecretNames() {
		secret, ok := desired.secrets[names.ID(stack.Namespace, name)]
		if !ok {
			return stackSecrets{}, fmt.Errorf("stack %s names secret %s, which its namespace does not have", stack.ID(), name)
		}
		opened, err := r.open(secret.Spec.Value)
		if err != nil {
			return stackSecrets{}, fmt.Errorf("open secret %s of stack %s: %w", name, stack.ID(), err)
		}
		values[name] = string(opened)
		secrets.digests[name] = secret.Spec.Digest
	}
	env, err := compose.RenderSecretEnv(stack.Name, stack.Spec, values)
	if err != nil {
		return stackSecrets{}, fmt.Errorf("render the secret variables of stack %s: %w", stack.ID(), err)
	}
	secrets.env = env
	for serviceName, service := range stack.Spec.Services {
		for _, name := range service.Secrets {
			secrets.files[name] = []byte(values[name])
		}
		if len(service.Secrets) > 0 {
			secrets.labels[serviceName] = secretLabel(r.digestKey, secrets.digests, service.Secrets)
		}
	}
	return secrets, nil
}

// writeSecrets writes the new and changed secret files and the secret
// variables before compose up. A file is replaced by a rename, so a running
// container keeps the old value until compose up recreates it.
func (r *Runner) writeSecrets(project string, secrets stackSecrets) error {
	for _, name := range slices.Sorted(maps.Keys(secrets.files)) {
		path, value := r.files.secretFile(project, name), secrets.files[name]
		if current, err := os.ReadFile(path); err == nil && bytes.Equal(current, value) {
			continue
		}
		// readable by all: dockerd bind-mounts it as root, and the container
		// reads it with its own user; the directories above are root's only
		if err := atomicfile.Write(path, value, 0o444); err != nil {
			return err
		}
	}
	if secrets.env != nil {
		return atomicfile.Write(r.files.secretEnvFile(project), secrets.env, 0o600)
	}
	return nil
}

// removeStaleSecrets removes, after compose up, what the stack no longer
// asks for: until then a running container may still use it.
func (r *Runner) removeStaleSecrets(project string, secrets stackSecrets) error {
	entries, err := os.ReadDir(r.files.secretFilesDir(project))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, entry := range entries {
		if _, asked := secrets.files[entry.Name()]; !asked {
			if err := os.Remove(filepath.Join(r.files.secretFilesDir(project), entry.Name())); err != nil {
				return err
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
// placed here and have no Compose file left (compose down ran): the
// leavers, and what a crash left behind.
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
