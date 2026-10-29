package state

import (
	"fmt"

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
	// StacksRevision is the state of the stacks the new cluster.yaml was
	// checked against: if a stack changed in between, the check no longer
	// holds and the command is refused.
	StacksRevision int64 `json:"stacks_revision"`
}

func applyConfig(s *State, _ Command, payload ApplyConfig) error {
	if problems := clusterdir.Check(payload.Spec); len(problems) > 0 {
		return problems
	}
	if payload.StacksRevision != s.StacksRevision {
		return fmt.Errorf("cluster.yaml was checked against stacks revision %d, but %d came in between; apply again",
			payload.StacksRevision, s.StacksRevision)
	}
	for _, name := range clusterdir.SecretNames(payload.Spec) {
		if _, ok := payload.Secrets[name]; !ok {
			return fmt.Errorf("secret %s is used in cluster.yaml but missing", name)
		}
	}
	s.Config = ClusterConfig{Revision: s.Config.Revision + 1, Hash: payload.Hash, Spec: payload.Spec.WithDefaults()}
	s.Secrets = payload.Secrets
	s.JoinKeys = payload.JoinKeys
	s.Registries = payload.Registries
	return nil
}
