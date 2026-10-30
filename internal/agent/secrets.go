package agent

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"github.com/webcodingcat/clowder/internal/atomicfile"
	"github.com/webcodingcat/clowder/internal/compose"
	"github.com/webcodingcat/clowder/internal/state"
)

// DefaultSecretsDir holds the stacks' secrets in clear. /run is a tmpfs:
// they never reach a disk, and a boot clears them. It is not a systemd
// RuntimeDirectory, which would go each time the agent stops while the
// containers still use it.
const DefaultSecretsDir = "/run/clowder-secrets"

// stackSecrets are what compose up of a stack needs of its secrets.
type stackSecrets struct {
	// files holds the value of each secret a service asks for as a file.
	files map[string][]byte
	// digests holds, per service that asks for secret files, the digest of
	// their values (compose.Target.SecretDigests).
	digests map[string]string
	// env is the second Compose file with the secret variables; nil if the
	// stack sets none.
	env []byte
}

// secretDigestPurpose names the key, derived from the data key, that the
// digests of secret files are made with.
const secretDigestPurpose = "clowder secrets label"

// secretDigest is an HMAC-SHA256 over the secrets of one service, in name
// order, each name and value preceded by its length, so no two sets of
// secrets give the same input.
func secretDigest(key []byte, values map[string]string, names []string) string {
	mac := hmac.New(sha256.New, key)
	for _, name := range slices.Sorted(slices.Values(names)) {
		writeParts(mac, []byte(name), []byte(values[name]))
	}
	return hex.EncodeToString(mac.Sum(nil))
}

// writeParts writes each part to a hash after its length, so no two lists
// of parts give the hash the same input.
func writeParts(hash io.Writer, parts ...[]byte) {
	for _, part := range parts {
		_, _ = hash.Write(binary.BigEndian.AppendUint64(nil, uint64(len(part)))) // a hash never fails to write
		_, _ = hash.Write(part)
	}
}

// openSecrets opens the sealed values of the secrets the stack asks for.
func (r *stackRunner) openSecrets(parsed *compose.Stack, sealed map[string]state.Sealed) (stackSecrets, error) {
	values := map[string]string{}
	for _, name := range parsed.SecretNames() {
		value, ok := sealed[name]
		if !ok {
			return stackSecrets{}, fmt.Errorf("stack %s asks for secret %s, which the cluster does not have", parsed.Name, name)
		}
		opened, err := r.open(value)
		if err != nil {
			return stackSecrets{}, fmt.Errorf("open secret %s of stack %s: %w", name, parsed.Name, err)
		}
		values[name] = string(opened)
	}
	env, err := compose.RenderSecretEnv(parsed, values)
	if err != nil {
		return stackSecrets{}, err
	}
	secrets := stackSecrets{files: map[string][]byte{}, digests: map[string]string{}, env: env}
	for serviceName, service := range parsed.Services {
		for _, name := range service.Secrets {
			secrets.files[name] = []byte(values[name])
		}
		if len(service.Secrets) > 0 {
			secrets.digests[serviceName] = secretDigest(r.digestKey, values, service.Secrets)
		}
	}
	return secrets, nil
}

// writeSecrets writes the new and changed secret files and the secret
// variables before compose up. A file is replaced by a rename, so a running
// container keeps the old value until compose up recreates it.
func (r *stackRunner) writeSecrets(project string, secrets stackSecrets) error {
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
func (r *stackRunner) removeStaleSecrets(project string, secrets stackSecrets) error {
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
func (r *stackRunner) removeSecretsOfOthers(desired desiredState) {
	entries, err := os.ReadDir(r.files.SecretsDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		r.logger.Error("list the stacks' secrets", "error", err)
		return
	}
	for _, entry := range entries {
		project := entry.Name()
		if _, staying := desired.stacks[project]; staying || clowdersOwn(project) {
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
