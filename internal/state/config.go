package state

import (
	"fmt"
	"maps"
	"slices"

	"github.com/webcodingcat/clowder/internal/clusterdir"
)

// ApplyConfig replaces cluster.yaml and the secrets with what the user
// applied. JoinKeys is the whole list of valid bootstrap keys: a key left
// out is invalid from now on.
type ApplyConfig struct {
	Hash       string            `json:"hash"`
	Spec       clusterdir.Config `json:"spec"`
	Secrets    map[string]Sealed `json:"secrets"`
	JoinKeys   []Sealed          `json:"join_keys"`
	Registries map[string]Sealed `json:"registries"`
	// Files are the files next to cluster.yaml that it names, by their
	// names in it (clusterdir.FileNames); none holds a secret.
	Files map[string][]byte `json:"files,omitempty"`
	// ResourcesRevision is the state of the resources the new cluster.yaml
	// was checked against: if one changed in between, the check no longer
	// holds and the command is refused.
	ResourcesRevision int64 `json:"resources_revision"`
}

func applyConfig(s *State, _ Command, payload ApplyConfig) error {
	if problems := clusterdir.Check(payload.Spec); len(problems) > 0 {
		return problems
	}
	if problems := clusterdir.CheckFiles(payload.Spec, payload.Files); len(problems) > 0 {
		return problems
	}
	if payload.ResourcesRevision != s.ResourcesRevision {
		return fmt.Errorf("cluster.yaml was checked against resources revision %d, but %d came in between; "+
			"apply again", payload.ResourcesRevision, s.ResourcesRevision)
	}
	for _, name := range clusterdir.SecretNames(payload.Spec) {
		if _, ok := payload.Secrets[name]; !ok {
			return fmt.Errorf("secret %s is used in cluster.yaml but missing", name)
		}
	}
	for _, id := range slices.Sorted(maps.Keys(s.Stacks)) {
		if s.Stacks[id].Privileged && !slices.Contains(payload.Spec.Stacks.Privileged, id) {
			return fmt.Errorf("stack %s asks for privileges, so it must stay in stacks.privileged", id)
		}
	}
	for _, id := range slices.Sorted(maps.Keys(s.Volumes)) {
		class := s.Volumes[id].Spec.Class
		if _, ok := payload.Spec.StorageClasses[class]; !ok {
			return fmt.Errorf("volume %s is of storage class %s, so the class must stay", id, class)
		}
	}
	// only the files cluster.yaml names: the payload may carry no others
	files := map[string][]byte{}
	for _, name := range clusterdir.FileNames(payload.Spec) {
		files[name] = payload.Files[name]
	}
	s.Config = ClusterConfig{
		Revision: s.Config.Revision + 1, Hash: payload.Hash, Spec: payload.Spec.WithDefaults(), Files: files,
	}
	s.ClusterSecrets = payload.Secrets
	s.JoinKeys = payload.JoinKeys
	s.Registries = payload.Registries
	return nil
}
