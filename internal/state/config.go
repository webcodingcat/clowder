package state

import (
	"fmt"
	"maps"
	"slices"

	"github.com/webcodingcat/clowder/internal/clusterspec"
)

// ApplyConfig replaces cluster.yaml and its values with what the user
// applied. Secrets holds a value for each name of the spec
// (clusterspec.SecretPlaces), and no other.
type ApplyConfig struct {
	Spec    clusterspec.Config      `json:"spec"`
	Secrets map[string]SealedSecret `json:"secrets"`
	// Files are the files next to cluster.yaml that it names, by their
	// names in it (clusterspec.FileNames); none holds a secret.
	Files map[string][]byte `json:"files,omitempty"`
}

// RegistryLogin is the login for one private image registry. The user name
// is no secret; only the password is sealed.
type RegistryLogin struct {
	Username string
	Password Sealed
}

// ClusterSecret is the sealed value cluster.yaml has under a name; false if
// it has none.
func (s *State) ClusterSecret(name clusterspec.SecretRef) (Sealed, bool) {
	secret, ok := s.ClusterSecrets[string(name)]
	return secret.Value, ok
}

// RegistryLogins are the logins for private image registries, by registry.
func (s *State) RegistryLogins() map[string]RegistryLogin {
	logins := map[string]RegistryLogin{}
	for host, registry := range s.Config.Spec.Registries {
		if password, ok := s.ClusterSecret(registry.Password); ok {
			logins[host] = RegistryLogin{Username: registry.Username, Password: password}
		}
	}
	return logins
}

// checkClusterSecrets makes sure the values match the spec: one value for
// each name the spec has, no value for a name it does not have, and a
// digest for each value. Check has already made sure that each name is
// the path of its place.
func checkClusterSecrets(spec clusterspec.Config, secrets map[string]SealedSecret) error {
	named := map[string]bool{}
	for _, place := range clusterspec.SecretPlaces(spec) {
		named[string(place.Name)] = true
		secret, ok := secrets[string(place.Name)]
		switch {
		case !ok:
			return fmt.Errorf("%s: the value is missing", place.Path)
		case secret.Digest == "":
			return fmt.Errorf("%s: the value has no digest", place.Path)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(secrets)) {
		if !named[name] {
			return fmt.Errorf("a value for %s, which cluster.yaml does not name", name)
		}
	}
	return nil
}

// applyConfig refuses a cluster.yaml that a resource of the cluster would
// no longer pass: a storage class a volume has, a priority category a
// stack names, a stack's place in stacks.privileged. The state checks it
// itself, in the log's order, so no resource can slip in between.
func applyConfig(s *State, _ Command, payload ApplyConfig) (func(), error) {
	if problems := clusterspec.Check(payload.Spec); len(problems) > 0 {
		return nil, problems
	}
	if problems := clusterspec.CheckFiles(payload.Spec, payload.Files); len(problems) > 0 {
		return nil, problems
	}
	if err := checkClusterSecrets(payload.Spec, payload.Secrets); err != nil {
		return nil, fmt.Errorf("cluster.yaml: %w", err)
	}
	if err := checkSecretLimits(s.Secrets, payload.Secrets); err != nil {
		return nil, fmt.Errorf("cluster.yaml: %w", err)
	}
	spec := payload.Spec.WithDefaults()
	for _, id := range slices.Sorted(maps.Keys(s.Stacks)) {
		stack := s.Stacks[id]
		if stack.Spec.Privileged() && !slices.Contains(spec.Stacks.Privileged, id) {
			return nil, fmt.Errorf("stack %s asks for privileges, so it must stay in stacks.privileged", id)
		}
		if _, err := servicesOf(stack.Spec, spec.Priorities); err != nil {
			return nil, fmt.Errorf("cluster.yaml would break stack %s: %w", id, err)
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
				return nil, fmt.Errorf("volume %s has storage class %s, so the class must stay", id, class)
			}
		}
	}
	// only the files cluster.yaml names: the payload may carry no others
	files := map[string][]byte{}
	for _, name := range clusterspec.FileNames(payload.Spec) {
		files[name] = payload.Files[name]
	}
	return func() {
		s.Config = ClusterConfig{Revision: s.Config.Revision + 1, Spec: spec, Files: files}
		s.ClusterSecrets = payload.Secrets
	}, nil
}
