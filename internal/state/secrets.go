package state

import (
	"fmt"
	"maps"
	"slices"

	"github.com/webcodingcat/clowder/internal/resource"
)

func findSecret(s *State, id string) *Meta {
	if secret, ok := s.Secrets[id]; ok {
		return &secret.Meta
	}
	return nil
}

// putSecret stores a secret's sealed value, within the limits of
// checkSecretLimits.
func putSecret(s *State, id string, payload PutResource) (func() *Meta, error) {
	spec, err := decodeSpec[SealedSecret](payload.Kind, id, payload.Spec)
	if err != nil {
		return nil, err
	}
	stored, exists := s.Secrets[id]
	if exists && stored.Spec.Digest == spec.Digest {
		return nil, errUnchanged
	}
	switch size := ClearSize(spec.Value); {
	case spec.Digest == "":
		return nil, fmt.Errorf("secret %s has no digest", id)
	case size < 0:
		return nil, fmt.Errorf("secret %s: the value is not sealed", id)
	case size == 0:
		return nil, fmt.Errorf("secret %s is empty", id)
	}
	put := &Secret{Meta: Meta{Namespace: payload.Namespace, Name: payload.Name, Owner: payload.Owner}, Spec: spec}
	if exists {
		put.Meta = stored.Meta
	}
	secrets := maps.Clone(s.Secrets)
	secrets[id] = put
	err = checkSecretLimits(secrets, s.ClusterSecrets)
	if err != nil {
		return nil, fmt.Errorf("secret %s: %w", id, err)
	}
	return func() *Meta {
		s.Secrets[id] = put
		return &put.Meta
	}, nil
}

// checkSecretLimits holds the limits on the secrets the cluster would keep:
// the namespaces' and the values of cluster.yaml. Every apply writes them
// into the Raft log and every snapshot holds them, so they are limited in
// clear (ClearSize) per value, per namespace and in the cluster, and in
// number. put-resource and apply-config both ask it, so neither can pass
// what the other refuses.
func checkSecretLimits(secrets map[string]*Secret, cluster map[string]SealedSecret) error {
	if count := len(secrets) + len(cluster); count > resource.MaxClusterSecrets {
		return fmt.Errorf("the cluster would have %d secrets, more than %d (cluster.yaml's values count too)",
			count, resource.MaxClusterSecrets)
	}
	inCluster := 0
	inNamespace := map[string]int{}
	add := func(what string, value Sealed) (int, error) {
		// what is too short to be sealed counts as nothing: putSecret
		// refuses such a resource, and the API seals cluster.yaml's values
		size := max(ClearSize(value), 0)
		if size > resource.MaxSecretBytes {
			return 0, fmt.Errorf("%s is larger than %d bytes", what, resource.MaxSecretBytes)
		}
		inCluster += size
		return size, nil
	}
	for _, id := range slices.Sorted(maps.Keys(secrets)) {
		secret := secrets[id]
		size, err := add("secret "+id, secret.Spec.Value)
		if err != nil {
			return err
		}
		inNamespace[secret.Namespace] += size
	}
	for _, name := range slices.Sorted(maps.Keys(cluster)) {
		if _, err := add(name, cluster[name].Value); err != nil {
			return err
		}
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

// removeSecret refuses a secret a stack names: the stack could not start
// again without it.
func removeSecret(s *State, id string) (func(), error) {
	secret := s.Secrets[id]
	if stack := s.stackNaming(secret.Namespace, secret.Name, "", func(stack *Stack) []string {
		return stack.Spec.SecretNames()
	}); stack != "" {
		return nil, fmt.Errorf("secret %s is stack %s's; change or delete the stack first", id, stack)
	}
	return func() { delete(s.Secrets, id) }, nil
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
