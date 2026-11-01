package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/webcodingcat/clowder/internal/clusterdir"
	"github.com/webcodingcat/clowder/internal/clusterspec"
	"github.com/webcodingcat/clowder/internal/state"
)

// ClusterApply is cluster.yaml with the secrets, as clowder cluster apply
// sends it (PUT /v1/cluster). The secrets travel in clear inside mutual TLS;
// the agent seals them before they go into the state.
type ClusterApply struct {
	Spec       clusterspec.Config                  `json:"spec"`
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
	Revision int64              `json:"revision"`
	Hash     string             `json:"hash"`
	Spec     clusterspec.Config `json:"spec"`
	Files    map[string][]byte  `json:"files,omitempty"`
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

// applyCluster seals the secrets and commits the new cluster.yaml. The
// state refuses one that a resource of the cluster would no longer pass.
func (s *Server) applyCluster(w http.ResponseWriter, r *http.Request) {
	var apply ClusterApply
	if err := json.NewDecoder(r.Body).Decode(&apply); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("read the request: %w", err))
		return
	}
	payload, err := s.sealApply(apply)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.write(w, r, state.KindApplyConfig, payload)
}

func (s *Server) sealApply(apply ClusterApply) (state.ApplyConfig, error) {
	hash, err := apply.Hash()
	if err != nil {
		return state.ApplyConfig{}, err
	}
	payload := state.ApplyConfig{
		Hash: hash, Spec: apply.Spec, Files: apply.Files,
		Secrets: map[string]state.Sealed{}, Registries: map[string]state.RegistryLogin{},
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
		password, err := s.config.Keys.Seal([]byte(login.Password))
		if err != nil {
			return payload, err
		}
		payload.Registries[registry] = state.RegistryLogin{Username: login.Username, Password: password}
	}
	return payload, nil
}
