package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"

	"github.com/webcodingcat/clowder/internal/clusterdir"
	"github.com/webcodingcat/clowder/internal/compose"
	"github.com/webcodingcat/clowder/internal/state"
)

// ClusterApply is cluster.yaml with the secrets, as clowder cluster apply
// sends it (PUT /v1/cluster). The secrets travel in clear inside mutual TLS;
// the agent seals them before they go into the state.
type ClusterApply struct {
	Spec       clusterdir.Config                   `json:"spec"`
	Secrets    map[string]string                   `json:"secrets"`
	JoinKeys   []string                            `json:"join_keys"`
	Registries map[string]clusterdir.RegistryLogin `json:"registries"`
	// Files are the files next to cluster.yaml that it names.
	Files map[string][]byte `json:"files,omitempty"`
}

// Hash identifies what is applied, secrets included, without showing them:
// clowder cluster diff compares it with the cluster's.
func (c ClusterApply) Hash() (string, error) {
	// json writes map keys sorted, so equal contents give equal bytes
	encoded, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

// ClusterInfo is the applied cluster.yaml and its files (GET /v1/cluster),
// without the secrets.
type ClusterInfo struct {
	Revision int64             `json:"revision"`
	Hash     string            `json:"hash"`
	Spec     clusterdir.Config `json:"spec"`
	Files    map[string][]byte `json:"files,omitempty"`
}

func (s *Server) cluster(w http.ResponseWriter, _ *http.Request) {
	var info ClusterInfo
	s.config.Cluster.Read(func(current *state.State) {
		info = ClusterInfo{
			Revision: current.Config.Revision, Hash: current.Config.Hash, Spec: current.Config.Spec,
			Files: current.Config.Files,
		}
	})
	writeJSON(w, http.StatusOK, info)
}

// applyCluster seals the secrets and commits the new cluster.yaml.
func (s *Server) applyCluster(w http.ResponseWriter, r *http.Request) {
	var apply ClusterApply
	if err := json.NewDecoder(r.Body).Decode(&apply); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("read the request: %w", err))
		return
	}
	stacksRevision, err := s.checkStacksAgainst(r.Context(), apply.Spec.WithDefaults())
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	payload, err := s.sealApply(apply)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	payload.StacksRevision = stacksRevision
	s.write(w, r, state.KindApplyConfig, payload)
}

// checkStacksAgainst refuses a cluster.yaml that a stack of the cluster
// would not pass: a storage class it uses, a priority category it names, its
// place in stacks.privileged must not go while the stack is there. It tells
// the stacks revision it checked, which the state compares at commit.
func (s *Server) checkStacksAgainst(ctx context.Context, config clusterdir.Config) (int64, error) {
	// what the check needs of a stack, read under the state's lock
	type stackSource struct{ namespace, name, source string }
	stacks := map[string]stackSource{}
	var revision int64
	s.config.Cluster.Read(func(current *state.State) {
		revision = current.StacksRevision
		for id, stack := range current.Stacks {
			stacks[id] = stackSource{namespace: stack.Namespace, name: stack.Name, source: stack.Source}
		}
	})
	var problems compose.Problems
	for _, id := range slices.Sorted(maps.Keys(stacks)) {
		stack := stacks[id]
		parsed, err := compose.Parse(ctx, stack.name, []byte(stack.source))
		if err != nil {
			continue // a stack the cluster cannot read is no reason to refuse cluster.yaml
		}
		if _, err := checkAgainstCluster(stack.namespace, parsed, config); err != nil {
			var stackProblems compose.Problems
			if !errors.As(err, &stackProblems) {
				return 0, err
			}
			for _, problem := range stackProblems {
				problems = append(problems, "stack "+id+": "+problem)
			}
		}
	}
	if len(problems) > 0 {
		return 0, fmt.Errorf("cluster.yaml would break stacks of the cluster: %w", problems)
	}
	return revision, nil
}

func (s *Server) sealApply(apply ClusterApply) (state.ApplyConfig, error) {
	hash, err := apply.Hash()
	if err != nil {
		return state.ApplyConfig{}, err
	}
	payload := state.ApplyConfig{
		Hash: hash, Spec: apply.Spec, Files: apply.Files,
		Secrets: map[string]state.Sealed{}, Registries: map[string]state.Sealed{},
	}
	for name, value := range apply.Secrets {
		if payload.Secrets[name], err = s.config.Keys.Seal([]byte(value)); err != nil {
			return payload, err
		}
	}
	for _, key := range apply.JoinKeys {
		sealed, err := s.config.Keys.Seal([]byte(key))
		if err != nil {
			return payload, err
		}
		payload.JoinKeys = append(payload.JoinKeys, sealed)
	}
	for registry, login := range apply.Registries {
		encoded, err := json.Marshal(login)
		if err != nil {
			return payload, err
		}
		if payload.Registries[registry], err = s.config.Keys.Seal(encoded); err != nil {
			return payload, err
		}
	}
	return payload, nil
}
