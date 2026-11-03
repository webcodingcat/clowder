package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"time"

	"filippo.io/age"

	"github.com/webcodingcat/clowder/internal/clusterspec"
	"github.com/webcodingcat/clowder/internal/digest"
	"github.com/webcodingcat/clowder/internal/secretvalue"
	"github.com/webcodingcat/clowder/internal/state"
)

// ClusterApply is cluster.yaml as clowder cluster apply sends it (PUT
// /v1/cluster): the spec, with the secrets' names where cluster.yaml has
// their values; the values by name, each still encrypted as cluster.yaml
// has it (secretvalue, for the cluster); and the files it names. The CLI
// never sees a value in clear; the agent opens them and seals them before
// they go into the state.
type ClusterApply struct {
	Spec      clusterspec.Config `json:"spec"`
	Encrypted map[string]string  `json:"encrypted"`
	// Files are the files next to cluster.yaml that it names.
	Files map[string][]byte `json:"files,omitempty"`
}

// ClusterInfo is the applied cluster.yaml and its files (GET /v1/cluster),
// with the secrets' names only.
type ClusterInfo struct {
	Revision int64              `json:"revision"`
	Spec     clusterspec.Config `json:"spec"`
	Files    map[string][]byte  `json:"files,omitempty"`
}

// ClusterDiff is the answer to a dry run of cluster apply (PUT
// /v1/cluster?dryRun=true): the revision applied, and what the new
// cluster.yaml would change: its top-level fields, its files, its secrets
// by name. A secret encrypted anew with the same value is no change.
type ClusterDiff struct {
	Revision int64    `json:"revision"`
	Fields   []string `json:"fields"`
	Files    []string `json:"files"`
	Secrets  []string `json:"secrets"`
}

// Encryption names the recipients every value is encrypted for (GET
// /v1/encryption): the cluster's key and the recovery key. The CLI asks for
// them each time it encrypts, rather than trusting a file someone may have
// changed.
type Encryption struct {
	Cluster  string `json:"cluster"`
	Recovery string `json:"recovery"`
}

// clusterSecretPurpose names the key for the digests of cluster.yaml's
// values. It differs from the key for Secret resources
// (secretResourcePurpose).
const clusterSecretPurpose = "clowder cluster secret digest"

func (s *Server) cluster(w http.ResponseWriter, _ *http.Request) {
	var info ClusterInfo
	s.config.Cluster.Read(func(current *state.State) {
		info = ClusterInfo{Revision: current.Config.Revision, Spec: current.Config.Spec, Files: current.Config.Files}
	})
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) encryption(w http.ResponseWriter, _ *http.Request) {
	identity, recovery, err := s.recipients()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, Encryption{Cluster: identity.Recipient().String(), Recovery: recovery.String()})
}

// recipients are the cluster's age key, which opens the values, and the
// recovery key's recipient: every value is encrypted for both.
func (s *Server) recipients() (*age.X25519Identity, *age.X25519Recipient, error) {
	identity, err := s.clusterIdentity()
	if err != nil {
		return nil, nil, err
	}
	var recovery string
	s.config.Cluster.Read(func(current *state.State) { recovery = current.RecoveryRecipient })
	if recovery == "" {
		return nil, nil, errors.New("the cluster has no recovery key")
	}
	recoveryRecipient, err := age.ParseX25519Recipient(recovery)
	if err != nil {
		return nil, nil, fmt.Errorf("the cluster's recovery key: %w", err)
	}
	return identity, recoveryRecipient, nil
}

// RekeyRequest is the body of POST /v1/encryption/rekey: values as
// secretvalue's armor, each with the place it stands at in its file.
type RekeyRequest struct {
	Values []PlacedValue `json:"values"`
}

// PlacedValue is a value and the place it must be for, as its file says:
// a Secret's ID, or secretvalue.Cluster in cluster.yaml. For is what
// secretvalue.OpenFor wants; the value's envelope must name the same place.
type PlacedValue struct {
	For       string `json:"for"`
	Encrypted string `json:"encrypted"`
}

// RekeyAnswer is the values of a RekeyRequest encrypted anew, in its order.
type RekeyAnswer struct {
	Values []string `json:"values"`
}

// rekey encrypts values anew for the cluster's recipients now, each for the
// place it was for, so every value reaches the recovery key: one made by
// hand with age may lack it, and after a new recovery key all do. A value
// not meant for where it stands is refused, as an apply would refuse it.
// The cluster opens the values and answers only with ciphertext; no value
// leaves it in clear. It changes nothing in the state, so only the log
// tells who rekeyed.
func (s *Server) rekey(w http.ResponseWriter, r *http.Request) {
	var request RekeyRequest
	if err := decodeStrict(r.Body, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	identity, recoveryRecipient, err := s.recipients()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	answer := RekeyAnswer{Values: make([]string, len(request.Values))}
	for index, value := range request.Values {
		opened, err := secretvalue.OpenFor(value.Encrypted, value.For, identity)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("value %d: %w", index+1, err))
			return
		}
		if answer.Values[index], err = secretvalue.Encrypt(opened, identity.Recipient(), recoveryRecipient); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	s.config.Logger.Info("values rekeyed", "actor", actorOf(identityOf(r)), "values", len(request.Values))
	writeJSON(w, http.StatusOK, answer)
}

// applyCluster opens and seals the values and commits the new cluster.yaml;
// the state refuses one that a resource of the cluster would no longer
// pass. A dry run tries it on a copy of this agent's state instead, with
// every check of a real apply, and answers what would change.
func (s *Server) applyCluster(w http.ResponseWriter, r *http.Request) {
	var apply ClusterApply
	if err := json.NewDecoder(r.Body).Decode(&apply); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("read the request: %w", err))
		return
	}
	identity, err := s.clusterIdentity()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	payload := state.ApplyConfig{Spec: apply.Spec, Files: apply.Files, Secrets: map[string]state.SealedSecret{}}
	for _, name := range slices.Sorted(maps.Keys(apply.Encrypted)) {
		value, err := secretvalue.OpenFor(apply.Encrypted[name], secretvalue.Cluster, identity)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("%s: %w", name, err))
			return
		}
		if payload.Secrets[name], err = SealClusterSecret(s.config.Keys, name, value.Bytes); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	if r.URL.Query().Get("dryRun") == "true" {
		s.diffCluster(w, r, payload)
		return
	}
	s.commitRequest(w, r, state.KindApplyConfig, payload)
}

// SealClusterSecret seals a value of cluster.yaml for the state, with its
// digest: an HMAC over its name and value with a key derived from the data
// key, so a diff tells a changed value from one encrypted anew without
// showing it.
func SealClusterSecret(keys Keys, name string, value []byte) (state.SealedSecret, error) {
	sealed, err := keys.Seal(value)
	if err != nil {
		return state.SealedSecret{}, fmt.Errorf("seal %s: %w", name, err)
	}
	key, err := keys.Derive(clusterSecretPurpose)
	if err != nil {
		return state.SealedSecret{}, fmt.Errorf("derive the digest key: %w", err)
	}
	return state.SealedSecret{Value: sealed, Digest: digest.HMAC(key, []byte(name), value)}, nil
}

// clusterIdentity opens the cluster's age key from the state: what every
// value is encrypted for.
func (s *Server) clusterIdentity() (*age.X25519Identity, error) {
	var sealed state.Sealed
	s.config.Cluster.Read(func(current *state.State) {
		if current.Keys != nil {
			sealed = current.Keys.AgeIdentity
		}
	})
	if sealed == nil {
		return nil, errors.New("the cluster is not bootstrapped yet")
	}
	opened, err := s.config.Keys.Open(sealed)
	if err != nil {
		return nil, fmt.Errorf("open the cluster's age key: %w", err)
	}
	identity, err := age.ParseX25519Identity(string(opened))
	if err != nil {
		return nil, fmt.Errorf("read the cluster's age key: %w", err)
	}
	return identity, nil
}

// diffCluster applies the config to a copy of the state, then answers what
// it changes; a refusal answers 409 with the state's reason, as a real
// apply would.
func (s *Server) diffCluster(w http.ResponseWriter, r *http.Request, payload state.ApplyConfig) {
	cmd, err := state.NewCommand(state.KindApplyConfig, time.Now().UTC(), actorOf(identityOf(r)), payload)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	var copied *state.State
	s.config.Cluster.Read(func(current *state.State) { copied, err = current.Copy() })
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// Apply replaces the config and the secrets, so these keep the applied
	applied, appliedSecrets := copied.Config, copied.ClusterSecrets
	if _, err := copied.Apply(copied.Index+1, cmd); err != nil {
		writeError(w, http.StatusConflict, errors.New(reason(err)))
		return
	}
	fields, err := changedFields(applied.Spec, copied.Config.Spec)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, ClusterDiff{
		Revision: applied.Revision, Fields: fields,
		Files:   changedKeys(applied.Files, copied.Config.Files, bytes.Equal),
		Secrets: changedKeys(appliedSecrets, copied.ClusterSecrets, func(a, b state.SealedSecret) bool { return a.Digest == b.Digest }),
	})
}

// changedFields lists the top-level keys of cluster.yaml whose values
// differ.
func changedFields(applied, next clusterspec.Config) ([]string, error) {
	fields := func(config clusterspec.Config) (map[string]json.RawMessage, error) {
		encoded, err := json.Marshal(config)
		if err != nil {
			return nil, fmt.Errorf("encode cluster.yaml: %w", err)
		}
		var byField map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &byField); err != nil {
			return nil, fmt.Errorf("split cluster.yaml into its fields: %w", err)
		}
		return byField, nil
	}
	before, err := fields(applied)
	if err != nil {
		return nil, err
	}
	after, err := fields(next)
	if err != nil {
		return nil, err
	}
	return changedKeys(before, after, func(a, b json.RawMessage) bool { return bytes.Equal(a, b) }), nil
}

// changedKeys lists, sorted, the keys that came, went or whose values
// differ by same.
func changedKeys[Value any](applied, next map[string]Value, same func(a, b Value) bool) []string {
	changed := []string{}
	for _, key := range slices.Sorted(maps.Keys(next)) {
		if before, ok := applied[key]; !ok || !same(before, next[key]) {
			changed = append(changed, key)
		}
	}
	for _, key := range slices.Sorted(maps.Keys(applied)) {
		if _, kept := next[key]; !kept {
			changed = append(changed, key)
		}
	}
	slices.Sort(changed)
	return changed
}
