package state

import (
	"fmt"
	"maps"
	"slices"

	"github.com/webcodingcat/clowder/internal/clusterspec"
)

// ApplyConfig replaces cluster.yaml and the secrets with what the user
// applied. JoinKeys is the whole list of valid bootstrap keys: a key left
// out is invalid from now on.
type ApplyConfig struct {
	Hash       string                   `json:"hash"`
	Spec       clusterspec.Config       `json:"spec"`
	Secrets    map[string]Sealed        `json:"secrets"`
	JoinKeys   []Sealed                 `json:"join_keys"`
	Registries map[string]RegistryLogin `json:"registries"`
	// Files are the files next to cluster.yaml that it names, by their
	// names in it (clusterspec.FileNames); none holds a secret.
	Files map[string][]byte `json:"files,omitempty"`
}

// RegistryLogin is the login for one private image registry. The user name
// is no secret; only the password is sealed.
type RegistryLogin struct {
	Username string `json:"username"`
	Password Sealed `json:"password"`
}

// ValidJoinKeys are the bootstrap keys hosts may join with, and the first
// user bootstrap with.
func (s *State) ValidJoinKeys() []Sealed {
	return s.JoinKeys
}

// RegistryLogins are the logins for private image registries, by registry.
// The map is the state's: readers do not change it.
func (s *State) RegistryLogins() map[string]RegistryLogin {
	return s.Registries
}

// clusterValues are the sealed values cluster.yaml brings: its named
// secrets, the join keys and the registries' passwords.
func clusterValues(secrets map[string]Sealed, joinKeys []Sealed, registries map[string]RegistryLogin) []Sealed {
	values := slices.Collect(maps.Values(secrets))
	values = append(values, joinKeys...)
	for _, login := range registries {
		values = append(values, login.Password)
	}
	return values
}

// applyConfig refuses a cluster.yaml that a resource of the cluster would
// no longer pass: a storage class a volume has, a priority category a
// stack names, a stack's place in stacks.privileged. The state checks it
// itself, in the log's order, so no resource can slip in between.
func applyConfig(s *State, _ Command, payload ApplyConfig) error {
	if problems := clusterspec.Check(payload.Spec); len(problems) > 0 {
		return problems
	}
	if problems := clusterspec.CheckFiles(payload.Spec, payload.Files); len(problems) > 0 {
		return problems
	}
	for _, name := range clusterspec.SecretNames(payload.Spec) {
		if _, ok := payload.Secrets[name]; !ok {
			return fmt.Errorf("secret %s is used in cluster.yaml but missing", name)
		}
	}
	err := checkSecretLimits(s.Secrets, clusterValues(payload.Secrets, payload.JoinKeys, payload.Registries))
	if err != nil {
		return fmt.Errorf("cluster.yaml: %w", err)
	}
	spec := payload.Spec.WithDefaults()
	for _, id := range slices.Sorted(maps.Keys(s.Stacks)) {
		stack := s.Stacks[id]
		if stack.Spec.Privileged() && !slices.Contains(spec.Stacks.Privileged, id) {
			return fmt.Errorf("stack %s asks for privileges, so it must stay in stacks.privileged", id)
		}
		if _, err := servicesOf(stack.Spec, spec.Priorities); err != nil {
			return fmt.Errorf("cluster.yaml would break stack %s: %w", id, err)
		}
	}
	// a volume's class, and the class each copy is in (a fallback of it)
	for _, id := range slices.Sorted(maps.Keys(s.Volumes)) {
		volume := s.Volumes[id]
		classes := []string{volume.Spec.Class}
		for _, volumeCopy := range volume.Copies {
			classes = append(classes, volumeCopy.Class)
		}
		for _, class := range classes {
			if _, ok := spec.StorageClasses[class]; !ok {
				return fmt.Errorf("volume %s has storage class %s, so the class must stay", id, class)
			}
		}
	}
	// only the files cluster.yaml names: the payload may carry no others
	files := map[string][]byte{}
	for _, name := range clusterspec.FileNames(payload.Spec) {
		files[name] = payload.Files[name]
	}
	s.Config = ClusterConfig{
		Revision: s.Config.Revision + 1, Hash: payload.Hash, Spec: spec, Files: files,
	}
	s.ClusterSecrets = payload.Secrets
	s.JoinKeys = payload.JoinKeys
	s.Registries = payload.Registries
	return nil
}
