package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/webcodingcat/clowder/internal/clusterspec"
	"github.com/webcodingcat/clowder/internal/digest"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/resource"
	"github.com/webcodingcat/clowder/internal/secretvalue"
	"github.com/webcodingcat/clowder/internal/stackspec"
	"github.com/webcodingcat/clowder/internal/state"
)

// ApplyRequest holds the resources in the order in which they are applied,
// which is their order in the files.
type ApplyRequest struct {
	Resources []resource.Object `json:"resources"`
	// Create refuses every resource that already exists, as kubectl create
	// does (clowder create secret).
	Create bool `json:"create,omitempty"`
}

// DeleteRequest holds the resources in the order in which they are
// deleted: a resource that uses another comes before it.
type DeleteRequest struct {
	Resources []resource.Ref `json:"resources"`
	// WithData must be set to delete a volume. The volume's data is deleted
	// with it and cannot be brought back.
	WithData bool `json:"with_data,omitempty"`
}

// The outcomes of a resource in an apply or a delete. A dry run gives the
// same answer as a real one, but commits nothing.
const (
	Applied   = "applied"
	Deleted   = "deleted"
	Unchanged = "unchanged"
	// Refused means that the state refused the resource as sent.
	Refused = "refused"
	// Failed means that the request did not commit (no leader, no quorum,
	// a timeout), and may succeed when tried again.
	Failed = "failed"
	// NotApplied marks every other resource of a request that was refused
	// or failed, because a request is applied whole or not at all.
	NotApplied = "not-applied"
)

// ChangeResult has one result per resource, in the order sent.
type ChangeResult struct {
	// Index is 0 if nothing was committed, as in a dry run.
	Index   uint64           `json:"index"`
	Results []ResourceResult `json:"results"`
	// Warnings are about the service URLs in the configuration that the
	// request applies (clusterspec.ServiceURLWarnings).
	Warnings []string `json:"warnings,omitempty"`
	// HetznerToken is what a dry run found out about the request's new
	// Hetzner token. It is nil if the request does not change the token.
	HetznerToken *TokenCheck `json:"hetzner_token,omitempty"`
}

// ResourceResult is the outcome for one resource.
type ResourceResult struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	Outcome   string `json:"outcome"`
	Message   string `json:"message,omitempty"`
	// Created and Differs are set in a dry run. Differs lists the spec's
	// top-level fields, or a Secret's keys (never its values).
	Created bool     `json:"created,omitempty"`
	Differs []string `json:"differs,omitempty"`
}

// TokenCheck is what an apply's new Hetzner token sees: the servers of
// Seen out of Hosts hosts. Error is set if Hetzner could not be asked.
type TokenCheck struct {
	Hosts int    `json:"hosts"`
	Seen  int    `json:"seen"`
	Error string `json:"error,omitempty"`
}

// secretResourcePurpose differs from the key for the containers' labels,
// so neither digest reveals anything about the other.
const secretResourcePurpose = "clowder secret resource digest"

// changeBatch is an apply or a delete as the agent that took it hands it
// to the leader. The leader decides, because a follower's state may be
// behind and answer "unchanged" or "refused" for what has changed since.
type changeBatch struct {
	DryRun  bool          `json:"dry_run,omitempty"`
	Actor   string        `json:"actor"`
	Changes []batchChange `json:"changes"`
}

type batchChange struct {
	Ref    resource.Ref          `json:"ref"`
	Change *state.ResourceChange `json:"change,omitempty"`
	// Error says why the resource has no change (its spec is wrong).
	Error string `json:"error,omitempty"`
}

// decodeStrict refuses an unknown field, so a typo does not become an
// empty request.
func decodeStrict(body io.Reader, request any) error {
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(request); err != nil {
		return fmt.Errorf("read the request: %w", err)
	}
	return nil
}

func (s *Server) applyResources(w http.ResponseWriter, r *http.Request) {
	var request ApplyRequest
	if err := decodeStrict(r.Body, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	batch := changeBatch{DryRun: r.URL.Query().Get("dryRun") == "true", Actor: actorOf(identityOf(r))}
	// refuse a resource named twice, such as a Secret and a SealedSecret
	// of the same name; the state would silently keep only the second
	sentAs := map[resource.Ref]string{}
	for _, object := range request.Resources {
		stored := resource.Ref{
			Kind: resource.StoredKind(object.Kind), Namespace: object.Metadata.Namespace, Name: object.Metadata.Name,
		}
		if kind, twice := sentAs[stored]; twice {
			batch.add(object.Ref(), state.ResourceChange{},
				fmt.Errorf("the request names %s twice (first as a %s)", stored, kind))
			continue
		}
		sentAs[stored] = object.Kind
		put, err := s.putChange(object, request.Create)
		batch.add(object.Ref(), state.ResourceChange{Put: &put}, err)
	}
	s.runOnLeader(w, r, batch)
}

func (s *Server) deleteResources(w http.ResponseWriter, r *http.Request) {
	var request DeleteRequest
	if err := decodeStrict(r.Body, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	// refuse the whole request before anything is deleted; the user
	// confirms the loss of data once for the request
	if index := slices.IndexFunc(request.Resources, func(ref resource.Ref) bool {
		return ref.Kind == resource.KindVolume
	}); index >= 0 && !request.WithData {
		volume := request.Resources[index]
		writeError(w, http.StatusBadRequest, fmt.Errorf("deleting volume %s deletes its data; confirm with --with-data",
			names.ID(volume.Namespace, volume.Name)))
		return
	}
	batch := changeBatch{DryRun: r.URL.Query().Get("dryRun") == "true", Actor: actorOf(identityOf(r))}
	for _, ref := range request.Resources {
		// delete -f of a SealedSecret deletes the Secret it is stored as
		change := state.ResourceChange{Delete: &state.DeleteResource{
			Kind: resource.StoredKind(ref.Kind), Namespace: ref.Namespace, Name: ref.Name, Owner: state.OwnerCLI,
		}}
		batch.add(ref, change, nil)
	}
	s.runOnLeader(w, r, batch)
}

func (b *changeBatch) add(ref resource.Ref, change state.ResourceChange, err error) {
	if err != nil {
		b.Changes = append(b.Changes, batchChange{Ref: ref, Error: err.Error()})
		return
	}
	b.Changes = append(b.Changes, batchChange{Ref: ref, Change: &change})
}

// putChange reads the spec strictly. Every change by a user comes from the
// CLI, so the owner is the channel cli (state.OwnerCLI).
func (s *Server) putChange(object resource.Object, create bool) (state.PutResource, error) {
	if object.APIVersion != resource.APIVersion {
		return state.PutResource{}, fmt.Errorf("apiVersion %q is not %s", object.APIVersion, resource.APIVersion)
	}
	put := state.PutResource{
		Kind: resource.StoredKind(object.Kind), Namespace: object.Metadata.Namespace, Name: object.Metadata.Name,
		Owner: state.OwnerCLI, Create: create,
	}
	id := names.ID(put.Namespace, put.Name)
	var spec any
	var err error
	switch object.Kind {
	case resource.KindClusterConfiguration:
		spec, err = resource.DecodeClusterConfiguration(object.Spec)
	case resource.KindNamespace:
		spec, err = resource.DecodeNamespaceSpec(object.Spec)
	case resource.KindVolume:
		spec, err = resource.DecodeVolumeSpec(object.Spec)
	case resource.KindStack:
		// read strictly here, because the state reads the spec leniently;
		// stackspec.Check runs in the state
		spec, err = stackspec.Decode(object.Spec)
	case resource.KindSecret:
		var values map[string][]byte
		if values, err = resource.DecodeSecretSpec(object.Spec); err == nil {
			spec, err = s.encryptSecret(id, values)
		}
	case resource.KindSealedSecret:
		var values map[string][]byte
		if values, err = s.unsealSecret(id, object.Spec); err == nil {
			spec, err = s.encryptSecret(id, values)
		}
	default:
		err = fmt.Errorf("no kind of resource %q", object.Kind)
	}
	if err != nil {
		return state.PutResource{}, err
	}
	if put.Spec, err = json.Marshal(spec); err != nil {
		return state.PutResource{}, fmt.Errorf("encode the spec: %w", err)
	}
	return put, nil
}

// unsealSecret requires each value to be sealed for this Secret. Otherwise
// a value copied from another Secret, such as the Hetzner token of
// clowder-system, could reach a container.
func (s *Server) unsealSecret(id string, raw json.RawMessage) (map[string][]byte, error) {
	written, err := resource.DecodeSealedSecretSpec(raw)
	if err != nil {
		return nil, err
	}
	identity, err := s.sealingKey()
	if err != nil {
		return nil, err
	}
	values := map[string][]byte{}
	for _, key := range slices.Sorted(maps.Keys(written.EncryptedData)) {
		value, err := secretvalue.OpenFor(written.EncryptedData[key], id, identity)
		if err != nil {
			return nil, fmt.Errorf("sealed secret %s, key %s: %w", id, key, err)
		}
		values[key] = value.Bytes
	}
	return values, nil
}

// encryptSecret computes the digest as an HMAC, because a plain hash would
// give away a short password. It includes the ID, so two secrets with the
// same values have different digests.
func (s *Server) encryptSecret(id string, values map[string][]byte) (state.EncryptedSecret, error) {
	digestKey, err := s.config.Keys.Derive(secretResourcePurpose)
	if err != nil {
		return state.EncryptedSecret{}, fmt.Errorf("derive the digest key: %w", err)
	}
	encrypted := state.EncryptedSecret{Data: map[string]state.Encrypted{}, KeyDigests: map[string]string{}}
	parts := [][]byte{[]byte(id)}
	for _, key := range slices.Sorted(maps.Keys(values)) {
		if encrypted.Data[key], err = s.config.Keys.Encrypt(values[key]); err != nil {
			return state.EncryptedSecret{}, fmt.Errorf("encrypt secret %s: %w", id, err)
		}
		parts = append(parts, []byte(key), values[key])
		encrypted.KeyDigests[key] = digest.HMAC(digestKey, []byte(id), []byte(key), values[key])
	}
	encrypted.Digest = digest.HMAC(digestKey, parts...)
	return encrypted, nil
}

func (s *Server) runOnLeader(w http.ResponseWriter, r *http.Request, batch changeBatch) {
	waiting, cancel := context.WithTimeout(r.Context(), waitTimeout)
	defer cancel()
	leader, here, err := s.config.waitForLeader(waiting)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err)
		return
	}
	var answer ChangeResult
	if here {
		answer = s.runChanges(r.Context(), batch)
	} else {
		if answer, err = s.forwardChanges(waiting, leader, batch); err != nil {
			var statusError *StatusError
			if errors.As(err, &statusError) {
				writeError(w, statusError.Status, errors.New(statusError.Message))
				return
			}
			writeError(w, http.StatusServiceUnavailable, err)
			return
		}
	}
	if answer.Index > 0 {
		w.Header().Set(IndexHeader, strconv.FormatUint(answer.Index, 10))
	}
	writeJSON(w, http.StatusOK, answer)
}

func (s *Server) forwardChanges(ctx context.Context, leader state.HostID, batch changeBatch) (ChangeResult, error) {
	body, err := json.Marshal(batch)
	if err != nil {
		return ChangeResult{}, fmt.Errorf("encode the batch: %w", err)
	}
	url := "https://" + s.config.APIAddress(leader) + "/v1/internal/changes"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return ChangeResult{}, fmt.Errorf("forward to the leader (host %d): %w", leader, err)
	}
	request.Header.Set(VersionHeader, s.config.Version)
	var answer ChangeResult
	if err := send(s.config.Peers.forwarder, request, &answer); err != nil {
		return ChangeResult{}, fmt.Errorf("forward to the leader (host %d): %w", leader, err)
	}
	return answer, nil
}

// internalChanges does not forward a batch again, like internalPropose: if
// another agent leads by now, the caller asks again.
func (s *Server) internalChanges(w http.ResponseWriter, r *http.Request) {
	// lenient, as in internalPropose: the follower may run the next release
	// and send fields that this one does not know
	var batch changeBatch
	if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("read the batch: %w", err))
		return
	}
	if !state.IsCLIActor(batch.Actor) {
		writeError(w, http.StatusForbidden, fmt.Errorf("a batch carries only the CLI's changes, not %s's", batch.Actor))
		return
	}
	if !s.config.Cluster.IsLeader() {
		writeError(w, http.StatusServiceUnavailable, errors.New("not the leader; try again"))
		return
	}
	writeJSON(w, http.StatusOK, s.runChanges(r.Context(), batch))
}

// runChanges first applies the batch to a copy of the state, so a refusal
// is answered at once, a batch that changes nothing is not proposed, and a
// dry run stops there.
func (s *Server) runChanges(ctx context.Context, batch changeBatch) ChangeResult {
	answer := ChangeResult{Results: make([]ResourceResult, len(batch.Changes))}
	payload := state.ChangeResources{}
	for index, each := range batch.Changes {
		answer.Results[index] = ResourceResult{
			Kind: each.Ref.Kind, Namespace: each.Ref.Namespace, Name: each.Ref.Name, Outcome: NotApplied,
		}
		if each.Change != nil {
			payload.Changes = append(payload.Changes, *each.Change)
		}
	}
	// report every invalid spec at once, so the user can fix them all in
	// one go
	wrong := false
	for index, each := range batch.Changes {
		if each.Change == nil {
			answer.Results[index].Outcome, answer.Results[index].Message = Refused, each.Error
			wrong = true
		}
	}
	if wrong {
		return answer
	}
	if len(payload.Changes) == 0 {
		return answer
	}
	cmd, err := state.NewCommand(state.KindChangeResources, time.Now().UTC(), batch.Actor, payload)
	if err != nil {
		return answer.failed(err)
	}
	// the current state is needed only to tell a dry run what differs
	var before, after *state.State
	s.config.Cluster.Read(func(current *state.State) {
		after = current.Clone()
		if batch.DryRun {
			before = current.Clone()
		}
	})
	// Apply takes the time from the command. The trial run stamps it with
	// the leader's clock, and proposeHere stamps the committed command
	// again. No check of a resource reads the time.
	changes, err := after.Apply(after.Index+1, cmd)
	if err != nil {
		return answer.refused(err)
	}
	answer.outcomes(changes)
	switch {
	case batch.DryRun:
		s.describe(ctx, &answer, changes, before, after)
		return answer
	case len(changes) == 0:
		return answer
	}
	ctx, cancel := context.WithTimeout(ctx, waitTimeout)
	defer cancel()
	index, err := s.config.proposeHere(ctx, cmd)
	var refused *state.Refused
	switch {
	case errors.As(err, &refused):
		return answer.refused(refused.Err)
	case err != nil:
		return answer.failed(err)
	}
	answer.Index = index
	answer.Warnings = configWarnings(changes, after)
	return answer
}

// storedRef is needed because the result keeps the kind that the user sent
// (a SealedSecret), while the state keeps the stored kind (a Secret).
func (r ResourceResult) storedRef() resource.Ref {
	return resource.Ref{Kind: resource.StoredKind(r.Kind), Namespace: r.Namespace, Name: r.Name}
}

func (a *ChangeResult) outcomes(changes []state.Change) {
	changed := map[resource.Ref]bool{}
	for _, change := range changes {
		changed[resource.Ref{Kind: change.Kind, Namespace: change.Namespace, Name: change.Name}] = true
	}
	for index := range a.Results {
		result := &a.Results[index]
		switch ref := result.storedRef(); {
		case !changed[ref]:
			result.Outcome = Unchanged
		case slices.ContainsFunc(changes, func(change state.Change) bool {
			return change.Deleted && change.Kind == ref.Kind && change.Namespace == ref.Namespace && change.Name == ref.Name
		}):
			result.Outcome = Deleted
		default:
			result.Outcome = Applied
		}
	}
}

// refused marks the first resource if the refusal names none.
func (a ChangeResult) refused(err error) ChangeResult {
	index := 0
	var changeError *state.ChangeError
	if errors.As(err, &changeError) {
		index, err = changeError.Index, changeError.Err
	}
	for each := range a.Results {
		a.Results[each].Outcome = NotApplied
	}
	if index < len(a.Results) {
		a.Results[index].Outcome, a.Results[index].Message = Refused, reason(err)
	}
	return a
}

func (a ChangeResult) failed(err error) ChangeResult {
	for each := range a.Results {
		a.Results[each].Outcome, a.Results[each].Message = Failed, err.Error()
	}
	return a
}

func configWarnings(changes []state.Change, after *state.State) []string {
	if slices.ContainsFunc(changes, func(change state.Change) bool {
		return change.Kind == resource.KindClusterConfiguration
	}) {
		return clusterspec.ServiceURLWarnings(after.Config.Spec)
	}
	return nil
}

func (s *Server) describe(ctx context.Context, answer *ChangeResult, changes []state.Change,
	before, after *state.State,
) {
	for index := range answer.Results {
		result := &answer.Results[index]
		if result.Outcome != Applied {
			continue
		}
		ref := result.storedRef()
		old, existed := objectOf(before, ref)
		updated, _ := objectOf(after, ref)
		result.Created = !existed
		switch {
		case !existed:
		case ref.Kind == resource.KindSecret:
			id := names.ID(ref.Namespace, ref.Name)
			result.Differs = secretDifferences(before.Secrets[id].Spec, after.Secrets[id].Spec)
		default:
			result.Differs = differences(old, updated)
		}
	}
	answer.Warnings = configWarnings(changes, after)
	answer.HetznerToken = s.checkNewToken(ctx, before, after)
}

// secretDifferences compares digests and never shows a value. It names the
// changed keys under data, also for a SealedSecret, because the cluster
// stores a SealedSecret's encryptedData as a Secret's data.
func secretDifferences(was, is state.EncryptedSecret) []string {
	var differs []string
	for _, key := range is.Keys() {
		switch old, ok := was.KeyDigests[key]; {
		case !ok:
			differs = append(differs, "data."+key+" (new)")
		case old != is.KeyDigests[key]:
			differs = append(differs, "data."+key)
		}
	}
	for _, key := range was.Keys() {
		if _, kept := is.Data[key]; !kept {
			differs = append(differs, "data."+key+" (gone)")
		}
	}
	slices.Sort(differs)
	return differs
}

func differences(old, updated resource.Object) []string {
	fields := func(object resource.Object) map[string]json.RawMessage {
		var byField map[string]json.RawMessage
		_ = json.Unmarshal(object.Spec, &byField) // cannot fail: the API encoded the spec
		return byField
	}
	was, is := fields(old), fields(updated)
	var differs []string
	for _, field := range slices.Sorted(maps.Keys(is)) {
		if !bytes.Equal(was[field], is[field]) {
			differs = append(differs, "spec."+field)
		}
	}
	for _, field := range slices.Sorted(maps.Keys(was)) {
		if _, kept := is[field]; !kept {
			differs = append(differs, "spec."+field)
		}
	}
	slices.Sort(differs)
	return differs
}

// checkNewToken returns nil if the batch does not change the token. It
// never refuses the batch, because a user may change the token on purpose
// before moving the servers to its project. A configuration always names a
// token: Check refuses one without, and the state one whose Secret is
// missing. So the new state lacks a token only before the first
// configuration.
func (s *Server) checkNewToken(ctx context.Context, before, after *state.State) *TokenCheck {
	newEncrypted, err := after.HetznerToken()
	if err != nil {
		return nil
	}
	newToken, err := s.config.Keys.Decrypt(newEncrypted)
	if err != nil {
		return &TokenCheck{Error: fmt.Sprintf("decrypt the new token: %v", err)}
	}
	if oldEncrypted, err := before.HetznerToken(); err == nil {
		if oldToken, err := s.config.Keys.Decrypt(oldEncrypted); err == nil && bytes.Equal(oldToken, newToken) {
			return nil
		}
	}
	check := &TokenCheck{}
	var servers []int64
	for _, host := range after.Hosts {
		if host.Phase != state.Removed {
			check.Hosts++
			servers = append(servers, host.ServerID)
		}
	}
	if s.config.HetznerServers == nil {
		check.Error = "this agent has no client of Hetzner's API"
		return check
	}
	seen, err := s.config.HetznerServers(ctx, after.Config.Spec.Hetzner.API, string(newToken))
	if err != nil {
		check.Error = err.Error()
		return check
	}
	for _, server := range servers {
		if slices.Contains(seen, server) {
			check.Seen++
		}
	}
	return check
}

// reason drops the command's kind, which Apply puts first, because the
// user named a resource, not a command.
func reason(err error) string {
	var refusal *state.CommandError
	if errors.As(err, &refusal) {
		err = refusal.Err
	}
	var changeError *state.ChangeError
	if errors.As(err, &changeError) {
		err = changeError.Err
	}
	return err.Error()
}
