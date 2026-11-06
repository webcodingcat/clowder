package state

import (
	"fmt"
	"maps"
	"slices"

	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/resource"
)

func findSecret(s *State, id string) *Meta {
	if secret, ok := s.Secrets[id]; ok {
		return &secret.Meta
	}
	return nil
}

// putSecret stores a secret's encrypted values, within the limits of
// checkSecretLimits. It keeps every key that a stack of its namespace or
// the configuration names, because whatever names a key would fail without
// it.
func putSecret(s *State, id string, payload PutResource) (func() *Meta, error) {
	spec, err := decodeSpec[EncryptedSecret](payload.Kind, id, payload.Spec)
	if err != nil {
		return nil, err
	}
	stored, exists := s.Secrets[id]
	if exists && stored.Spec.Digest == spec.Digest {
		return nil, errUnchanged
	}
	switch {
	case spec.Digest == "":
		return nil, fmt.Errorf("secret %s has no digest", id)
	case len(spec.Data) == 0:
		return nil, fmt.Errorf("secret %s has no keys", id)
	}
	for _, key := range spec.Keys() {
		switch size := ClearSize(spec.Data[key]); {
		case !names.ValidSecretKey(key):
			return nil, fmt.Errorf("secret %s: key %q: %s", id, key, names.SecretKeyRule)
		case size < 0:
			return nil, fmt.Errorf("secret %s: the value of %s is not encrypted", id, key)
		case size == 0:
			return nil, fmt.Errorf("secret %s: the value of %s is empty", id, key)
		}
	}
	if err := s.checkKeysKept(payload.Namespace, payload.Name, spec.Data); err != nil {
		return nil, err
	}
	put := &Secret{Meta: Meta{Namespace: payload.Namespace, Name: payload.Name, Owner: payload.Owner}, Spec: spec}
	if exists {
		put.Meta = stored.Meta
	}
	secrets := maps.Clone(s.Secrets)
	secrets[id] = put
	if err := checkSecretLimits(secrets); err != nil {
		return nil, fmt.Errorf("secret %s: %w", id, err)
	}
	return func() *Meta {
		s.Secrets[id] = put
		return &put.Meta
	}, nil
}

// checkKeysKept refuses a secret that would lack a key that a stack of its
// namespace or the configuration names. A nil keep means that the secret is
// deleted.
func (s *State) checkKeysKept(namespace, name string, keep map[string]Encrypted) error {
	id := names.ID(namespace, name)
	if namespace == names.System {
		if place, named := s.configNaming(name, keep); named {
			if keep == nil {
				return fmt.Errorf("secret %s is named by %s of the configuration; change the configuration first",
					id, place.Path)
			}
			return fmt.Errorf("secret %s must keep the key %s, which %s of the configuration names",
				id, place.Ref.Key, place.Path)
		}
	}
	for _, stackID := range slices.Sorted(maps.Keys(s.Stacks)) {
		stack := s.Stacks[stackID]
		if stack.Namespace != namespace {
			continue
		}
		if keep == nil && slices.Contains(stack.Spec.SecretNames(), name) {
			return fmt.Errorf("secret %s is stack %s's; change or delete the stack first", id, stackID)
		}
		for _, ref := range stack.Spec.SecretKeyRefs() {
			if _, kept := keep[ref.Key]; ref.Name == name && !kept {
				return fmt.Errorf("secret %s must keep the key %s, which stack %s names", id, ref.Key, stackID)
			}
		}
	}
	return nil
}

// checkSecretLimits enforces the limits on the secrets that the cluster
// would keep. Every apply writes them into the Raft log, and every snapshot
// holds them. So their clear size (ClearSize) is limited per secret, per
// namespace and in the whole cluster, and so is their number.
func checkSecretLimits(secrets map[string]*Secret) error {
	if count := len(secrets); count > resource.MaxClusterSecrets {
		return fmt.Errorf("the cluster would have %d secrets, more than %d", count, resource.MaxClusterSecrets)
	}
	inCluster := 0
	inNamespace := map[string]int{}
	for _, id := range slices.Sorted(maps.Keys(secrets)) {
		secret := secrets[id]
		size := 0
		for _, value := range secret.Spec.Data {
			// what is too short to be encrypted counts as nothing: putSecret
			// refuses such a resource
			size += max(ClearSize(value), 0)
		}
		if size > resource.MaxSecretBytes {
			return fmt.Errorf("secret %s is larger than %d bytes", id, resource.MaxSecretBytes)
		}
		inCluster += size
		inNamespace[secret.Namespace] += size
	}
	for _, namespace := range slices.Sorted(maps.Keys(inNamespace)) {
		if inNamespace[namespace] > resource.MaxNamespaceSecretBytes {
			return fmt.Errorf("the secrets of namespace %s would be larger than %d bytes",
				namespace, resource.MaxNamespaceSecretBytes)
		}
	}
	if inCluster > resource.MaxClusterSecretBytes {
		return fmt.Errorf("the secrets of the cluster would be larger than %d bytes", resource.MaxClusterSecretBytes)
	}
	return nil
}

// removeSecret refuses a secret that a stack or the configuration names.
// The stack could not start again without it, and the configuration's
// values keep fencing, the Floating IP and the alerts working.
func removeSecret(s *State, id string) (func(), error) {
	secret := s.Secrets[id]
	if err := s.checkKeysKept(secret.Namespace, secret.Name, nil); err != nil {
		return nil, err
	}
	return func() { delete(s.Secrets, id) }, nil
}

// The length of what age adds when it encrypts for the cluster's one X25519
// recipient (datakey.Key.Encrypt).
const (
	// encryptedHeader is the header ("age-encryption.org/v1", one recipient
	// stanza, the MAC) and the payload's nonce.
	encryptedHeader = 168 + 16
	// encryptedChunk is how much clear text age encrypts at a time; each
	// chunk gets a tag of encryptedTag bytes.
	encryptedChunk = 64 << 10
	encryptedTag   = 16
)

// ClearSize returns the clear length of an encrypted value, worked out from
// the encrypted length, so the state can enforce the limits without
// decrypting the value. It returns -1 for what is too short to be
// encrypted. A test in datakey compares it with real encryption, so a
// change in age's format fails there.
func ClearSize(encrypted Encrypted) int {
	body := len(encrypted) - encryptedHeader
	if body < encryptedTag {
		return -1
	}
	chunks := (body + encryptedChunk + encryptedTag - 1) / (encryptedChunk + encryptedTag)
	return body - chunks*encryptedTag
}
