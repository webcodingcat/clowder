package state

import (
	"fmt"

	"github.com/webcodingcat/clowder/internal/resource"
)

func findSecret(s *State, id string) *Meta {
	if secret, ok := s.Secrets[id]; ok {
		return &secret.Meta
	}
	return nil
}

// putSecret stores a secret's sealed value. The limits are on the values in
// clear (ClearSize): per value, per namespace and in the cluster, as every
// apply writes them into the Raft log and every snapshot holds them.
func putSecret(s *State, id string, payload PutResource) (*Meta, error) {
	spec, err := decodeSpec[SealedSecret](payload.Kind, id, payload.Spec)
	if err != nil {
		return nil, err
	}
	stored, exists := s.Secrets[id]
	if exists && stored.Spec.Digest == spec.Digest {
		return nil, errUnchanged
	}
	size := ClearSize(spec.Value)
	switch {
	case spec.Digest == "":
		return nil, fmt.Errorf("secret %s has no digest", id)
	case size < 0:
		return nil, fmt.Errorf("secret %s: the value is not sealed", id)
	case size == 0:
		return nil, fmt.Errorf("secret %s is empty", id)
	case size > resource.MaxSecretBytes:
		return nil, fmt.Errorf("secret %s is larger than %d bytes", id, resource.MaxSecretBytes)
	}
	inNamespace, inCluster := size, size
	for otherID, other := range s.Secrets {
		if otherID == id {
			continue
		}
		inCluster += ClearSize(other.Spec.Value)
		if other.Namespace == payload.Namespace {
			inNamespace += ClearSize(other.Spec.Value)
		}
	}
	switch {
	case inNamespace > resource.MaxNamespaceSecretBytes:
		return nil, fmt.Errorf("secret %s: the secrets of namespace %s would be larger than %d bytes",
			id, payload.Namespace, resource.MaxNamespaceSecretBytes)
	case inCluster > resource.MaxClusterSecretBytes:
		return nil, fmt.Errorf("secret %s: the secrets of the cluster would be larger than %d bytes",
			id, resource.MaxClusterSecretBytes)
	}
	if !exists {
		stored = &Secret{Meta: Meta{Namespace: payload.Namespace, Name: payload.Name, Owner: payload.Owner}}
		s.Secrets[id] = stored
	}
	stored.Spec = spec
	return &stored.Meta, nil
}

// removeSecret refuses a secret a stack names: the stack could not start
// again without it.
func removeSecret(s *State, id string) error {
	secret := s.Secrets[id]
	if stack := s.stackNaming(secret.Namespace, secret.Name, "", func(stack *Stack) []string {
		return stack.Spec.SecretNames()
	}); stack != "" {
		return fmt.Errorf("secret %s is stack %s's; change or delete the stack first", id, stack)
	}
	delete(s.Secrets, id)
	return nil
}

// The length of what age adds when it seals for the cluster's one X25519
// recipient (datakey.Key.Seal).
const (
	// sealedHeader is the header ("age-encryption.org/v1", one recipient
	// stanza, the MAC) and the payload's nonce.
	sealedHeader = 168 + 16
	// sealedChunk is how much clear text age seals at a time; each chunk
	// gets a tag of sealedTag bytes.
	sealedChunk = 64 << 10
	sealedTag   = 16
)

// ClearSize is how long a sealed value is in clear, worked out from its
// length, so the state can hold the limits without opening the value; -1
// for what is too short to be sealed. A test in datakey holds it against
// real sealing, so a change in age's format fails there.
func ClearSize(sealed Sealed) int {
	body := len(sealed) - sealedHeader
	if body < sealedTag {
		return -1
	}
	chunks := (body + sealedChunk + sealedTag - 1) / (sealedChunk + sealedTag)
	return body - chunks*sealedTag
}
