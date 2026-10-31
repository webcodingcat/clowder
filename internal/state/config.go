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
	Hash       string             `json:"hash"`
	Spec       clusterspec.Config `json:"spec"`
	Secrets    map[string]Sealed  `json:"secrets"`
	JoinKeys   []Sealed           `json:"join_keys"`
	Registries map[string]Sealed  `json:"registries"`
	// Files are the files next to cluster.yaml that it names, by their
	// names in it (clusterspec.FileNames); none holds a secret.
	Files map[string][]byte `json:"files,omitempty"`
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
