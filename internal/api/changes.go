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

// ApplyRequest is the body of POST /v1/apply: the resources in the order
// in which they are applied, as the files have them.
type ApplyRequest struct {
	Resources []resource.Object `json:"resources"`
}

// DeleteRequest is the body of POST /v1/delete: the resources in the order
// in which they are deleted (what uses a resource comes before the
// resource).
type DeleteRequest struct {
	Resources []resource.Ref `json:"resources"`
	// WithData must be set to delete a volume. The volume's data is deleted
	// with it, and no later step can bring it back.
	WithData bool `json:"with_data,omitempty"`
}

// The outcomes of a resource in an apply or a delete. A dry run gives the
// same answer as a real one, without committing.
const (
	Applied   = "applied"
	Deleted   = "deleted"
	Unchanged = "unchanged"
	// Refused means that the state said no: the resource as sent will not
	// go in, and so nothing of the request goes in.
	Refused = "refused"
	// Failed means that the request did not commit (no leader, no quorum,
	// a timeout). The same request may go through when tried again.
	Failed = "failed"
	// NotApplied is every other resource of a request that was refused or
	// failed, because a request goes in whole or not at all.
	NotApplied = "not-applied"
)

// ChangeResult is the answer to POST /v1/apply and /v1/delete: one result
// per resource, in the order sent.
type ChangeResult struct {
	// Index is the Raft index of the committed command, or 0 if none was
	// committed, as in a dry run.
	Index   uint64           `json:"index"`
	Results []ResourceResult `json:"results"`
	// Warnings are about the service URLs of the configuration that the
	// request applies (clusterspec.ServiceURLWarnings).
	Warnings []string `json:"warnings,omitempty"`
	// HetznerToken is what a dry run found out about the new Hetzner token
	// of the request, or nil if the request does not change the token.
	HetznerToken *TokenCheck `json:"hetzner_token,omitempty"`
}

// ResourceResult is what became of one resource.
type ResourceResult struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	Outcome   string `json:"outcome"`
	Message   string `json:"message,omitempty"`
	// Created and Differs tell, in a dry run, what an applied resource
	// changes: either it is new, or these parts of it differ (the spec's
	// top-level fields, or a Secret's keys, never its values).
	Created bool     `json:"created,omitempty"`
	Differs []string `json:"differs,omitempty"`
}

// TokenCheck is what the new Hetzner token of an apply sees: the servers
// of Seen of the Hosts hosts, or Error if Hetzner could not be asked.
type TokenCheck struct {
	Hosts int    `json:"hosts"`
	Seen  int    `json:"seen"`
	Error string `json:"error,omitempty"`
}

// secretResourcePurpose names the key for the digests of Secret resources.
// It differs from the key for the containers' labels, so neither digest
// reveals anything about the other.
const secretResourcePurpose = "clowder secret resource digest"

// changeBatch is an apply or a delete in the form in which the agent that
// took it hands it to the leader: each resource's change in order, or why
// it has none. The leader decides on its own state. A follower's state may
// be behind, so it could answer "unchanged" or "refused" for a resource
// that has changed since.
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

// decodeStrict reads a request body. A field that the request does not
// have is an error, so a typo does not become an empty request.
func decodeStrict(body io.Reader, request any) error {
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(request); err != nil {
		return fmt.Errorf("read the request: %w", err)
	}
	return nil
}

// applyResources puts resources, in the order sent.
func (s *Server) applyResources(w http.ResponseWriter, r *http.Request) {
	var request ApplyRequest
	if err := decodeStrict(r.Body, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	batch := changeBatch{DryRun: r.URL.Query().Get("dryRun") == "true", Actor: actorOf(identityOf(r))}
	for _, object := range request.Resources {
		put, err := s.putChange(object)
		batch.add(object.Ref(), state.ResourceChange{Put: &put}, err)
	}
	s.runOnLeader(w, r, batch)
}

// deleteResources deletes resources, in the order sent.
func (s *Server) deleteResources(w http.ResponseWriter, r *http.Request) {
	var request DeleteRequest
	if err := decodeStrict(r.Body, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	// refused as a whole, before anything is deleted: the user confirms
	// the loss of the data once for the request
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
		change := state.ResourceChange{Delete: &state.DeleteResource{
			Kind: ref.Kind, Namespace: ref.Namespace, Name: ref.Name, Owner: state.OwnerCLI,
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

// putChange turns a resource as the user wrote it into its put. It reads
// the spec strictly and checks it, and for a secret it seals the values and
// computes the digest. Every change of a user comes from the CLI, so its
// owner is the channel cli (state.OwnerCLI).
func (s *Server) putChange(object resource.Object) (state.PutResource, error) {
	if object.APIVersion != resource.APIVersion {
		return state.PutResource{}, fmt.Errorf("apiVersion %q is not %s", object.APIVersion, resource.APIVersion)
	}
	put := state.PutResource{
		Kind: object.Kind, Namespace: object.Metadata.Namespace, Name: object.Metadata.Name, Owner: state.OwnerCLI,
	}
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
		spec, err = s.sealSecret(names.ID(put.Namespace, put.Name), object.Spec)
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

// sealSecret decrypts a secret's values, each of which must be encrypted
// for this Secret, seals them and computes the digest. The digest is an
// HMAC over the secret's ID and its keys and values, with a key derived
// from the data key. A plain hash would give away a short password. With
// the ID included, two secrets with the same values have different
// digests.
func (s *Server) sealSecret(id string, raw json.RawMessage) (state.SealedSecret, error) {
	written, err := resource.DecodeSecretSpec(raw)
	if err != nil {
		return state.SealedSecret{}, err
	}
	identity, err := s.clusterIdentity()
	if err != nil {
		return state.SealedSecret{}, err
	}
	digestKey, err := s.config.Keys.Derive(secretResourcePurpose)
	if err != nil {
		return state.SealedSecret{}, fmt.Errorf("derive the digest key: %w", err)
	}
	sealed := state.SealedSecret{Data: map[string]state.Sealed{}, KeyDigests: map[string]string{}}
	parts := [][]byte{[]byte(id)}
	for _, key := range slices.Sorted(maps.Keys(written.Encrypted)) {
		// the value must be encrypted for this Secret: one copied from
		// another, such as the Hetzner token of clowder-system, would
		// otherwise reach a container
		value, err := secretvalue.OpenFor(written.Encrypted[key], id, identity)
		if err != nil {
			return state.SealedSecret{}, fmt.Errorf("secret %s, key %s: %w", id, key, err)
		}
		if sealed.Data[key], err = s.config.Keys.Seal(value.Bytes); err != nil {
			return state.SealedSecret{}, fmt.Errorf("seal secret %s: %w", id, err)
		}
		parts = append(parts, []byte(key), value.Bytes)
		sealed.KeyDigests[key] = digest.HMAC(digestKey, []byte(id), []byte(key), value.Bytes)
	}
	sealed.Digest = digest.HMAC(digestKey, parts...)
	return sealed, nil
}

// runOnLeader has the leader run a batch: here if this agent leads, or else
// through the leader's /v1/internal/changes.
func (s *Server) runOnLeader(w http.ResponseWriter, r *http.Request, batch changeBatch) {
	leader, here, err := s.waitForLeader(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err)
		return
	}
	var answer ChangeResult
	if here {
		answer = s.runChanges(r.Context(), batch)
	} else {
		if answer, err = s.forwardChanges(r.Context(), leader, batch); err != nil {
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

// forwardChanges hands a batch to the leader and returns its answer.
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
	if err := send(s.forwarder, request, &answer); err != nil {
		return ChangeResult{}, fmt.Errorf("forward to the leader (host %d): %w", leader, err)
	}
	return answer, nil
}

// internalChanges runs a batch that a follower forwards for the CLI. Like
// internalPropose, it accepts only the CLI's batches and does not forward
// them again: if the lead has moved meanwhile, the caller asks again.
func (s *Server) internalChanges(w http.ResponseWriter, r *http.Request) {
	// not strict, as internalPropose: the follower may be of the next release,
	// and its changes may carry fields this one does not know
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

// runChanges runs a batch on the leader as one change-resources command.
// It first runs the batch on a copy of the state, with every check of the
// state. So a refusal is answered at once, an unchanged batch is not
// proposed, and a dry run ends there. Only then is the batch proposed; the
// state checks it once more as it commits.
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
	// report every spec that is wrong on its own at once, so the user
	// can fix them all in one go
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
		if after, err = current.Copy(); err == nil && batch.DryRun {
			before, err = current.Copy()
		}
	})
	if err != nil {
		return answer.failed(err)
	}
	// Apply takes the time from the command, which the trial run stamps
	// with the leader's clock; proposeHere stamps the committed command
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
	index, err := s.proposeHere(ctx, cmd)
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

// outcomes sets the outcome of each resource from what the batch changed.
// A resource that the batch did not change is unchanged.
func (a *ChangeResult) outcomes(changes []state.Change) {
	changed := map[resource.Ref]bool{}
	for _, change := range changes {
		changed[resource.Ref{Kind: change.Kind, Namespace: change.Namespace, Name: change.Name}] = true
	}
	for index := range a.Results {
		result := &a.Results[index]
		switch ref := (resource.Ref{Kind: result.Kind, Namespace: result.Namespace, Name: result.Name}); {
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

// refused marks the resource that the state refused, or the first one if
// the refusal names none. Nothing of the batch went in.
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

// failed marks every resource as failed, because the batch did not commit.
func (a ChangeResult) failed(err error) ChangeResult {
	for each := range a.Results {
		a.Results[each].Outcome, a.Results[each].Message = Failed, err.Error()
	}
	return a
}

// configWarnings warns about the configuration if the batch changed it.
func configWarnings(changes []state.Change, after *state.State) []string {
	if slices.ContainsFunc(changes, func(change state.Change) bool {
		return change.Kind == resource.KindClusterConfiguration
	}) {
		return clusterspec.ServiceURLWarnings(after.Config.Spec)
	}
	return nil
}

// describe adds to a dry run what an apply would change: which resources
// are new, which parts of the others differ, the configuration's warnings,
// and what a new Hetzner token sees.
func (s *Server) describe(ctx context.Context, answer *ChangeResult, changes []state.Change,
	before, after *state.State,
) {
	for index := range answer.Results {
		result := &answer.Results[index]
		if result.Outcome != Applied {
			continue
		}
		ref := resource.Ref{Kind: result.Kind, Namespace: result.Namespace, Name: result.Name}
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

// secretDifferences lists the keys of a Secret that were added, removed or
// whose value changed. It compares the digests of the values and never
// shows a value.
func secretDifferences(was, is state.SealedSecret) []string {
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

// differences lists the top-level fields of the spec that differ.
func differences(old, updated resource.Object) []string {
	fields := func(object resource.Object) map[string]json.RawMessage {
		var byField map[string]json.RawMessage
		_ = json.Unmarshal(object.Spec, &byField) // a spec the API encoded
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

// checkNewToken asks Hetzner once which servers the batch's new Hetzner
// token sees. It returns nil if the batch does not change the token. It
// never refuses, because a token may be changed on purpose before its
// project is.
func (s *Server) checkNewToken(ctx context.Context, before, after *state.State) *TokenCheck {
	newSealed, err := after.HetznerToken()
	if err != nil {
		return nil
	}
	newToken, err := s.config.Keys.Open(newSealed)
	if err != nil {
		return &TokenCheck{Error: fmt.Sprintf("open the new token: %v", err)}
	}
	if oldSealed, err := before.HetznerToken(); err == nil {
		if oldToken, err := s.config.Keys.Open(oldSealed); err == nil && bytes.Equal(oldToken, newToken) {
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

// reason returns why the state refused a command, without the command's
// kind that Apply puts first, because the user named a resource, not a
// command.
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
