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

	"github.com/webcodingcat/clowder/internal/digest"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/resource"
	"github.com/webcodingcat/clowder/internal/secretvalue"
	"github.com/webcodingcat/clowder/internal/stackspec"
	"github.com/webcodingcat/clowder/internal/state"
)

// ApplyRequest is the body of POST /v1/apply: the resources in the order
// they go in (namespaces first).
type ApplyRequest struct {
	Resources []resource.Object `json:"resources"`
}

// DeleteRequest is the body of POST /v1/delete: the resources in the order
// they go (what uses a resource before the resource).
type DeleteRequest struct {
	Resources []resource.Ref `json:"resources"`
	// WithData must be set to delete a volume: its data goes with it, and no
	// later step can bring it back.
	WithData bool `json:"with_data,omitempty"`
}

// The outcomes of a resource in an apply or a delete. A dry run answers
// what a real one would, without committing.
const (
	Applied   = "applied"
	Deleted   = "deleted"
	Unchanged = "unchanged"
	// Refused is the state's no: the resource as sent will not go in.
	Refused = "refused"
	// Failed did not commit (no leader, no quorum, a timeout): the same
	// request may go through when tried again.
	Failed = "failed"
	// NotTried follows a refusal or a failure: what comes after usually
	// needs what went wrong, so the request stops there.
	NotTried = "not-tried"
)

// ChangeResult is the answer to POST /v1/apply and /v1/delete: one result
// per resource, in the order sent.
type ChangeResult struct {
	// Index is the Raft index of the last command committed; 0 if none was,
	// as in a dry run.
	Index   uint64           `json:"index"`
	Results []ResourceResult `json:"results"`
}

// ResourceResult is what became of one resource.
type ResourceResult struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	Outcome   string `json:"outcome"`
	Message   string `json:"message,omitempty"`
}

// secretResourcePurpose names the key for the digests of Secret resources.
// It differs from the key for the containers' labels, so neither digest
// tells anything about the other.
const secretResourcePurpose = "clowder secret resource digest"

// changeBatch is an apply or a delete as the agent that took it hands it to
// the leader: each resource's command in order, or why it has none. The
// leader decides, on its own state. A follower's state may be behind: it
// could answer "unchanged" or "refused" for a resource that has changed
// since.
type changeBatch struct {
	DryRun  bool          `json:"dry_run,omitempty"`
	Changes []batchChange `json:"changes"`
}

type batchChange struct {
	Ref     resource.Ref   `json:"ref"`
	Command *state.Command `json:"command,omitempty"`
	// Error says why the resource has no command (its spec is wrong).
	Error string `json:"error,omitempty"`
}

// changeKinds are the commands a batch may carry, each a user's.
var changeKinds = map[string]bool{state.KindPutResource: true, state.KindDeleteResource: true}

// decodeStrict reads a request body; a field the request does not have is
// an error, so a typo is not an empty request.
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
	actor := actorOf(identityOf(r))
	batch := changeBatch{DryRun: r.URL.Query().Get("dryRun") == "true"}
	for _, object := range request.Resources {
		cmd, err := s.putCommand(actor, object)
		batch.add(object.Ref(), cmd, err)
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
	// refused as a whole, before anything goes: a delete of a directory would
	// otherwise take the stack and then stop at its volume
	if index := slices.IndexFunc(request.Resources, func(ref resource.Ref) bool {
		return ref.Kind == resource.KindVolume
	}); index >= 0 && !request.WithData {
		volume := request.Resources[index]
		writeError(w, http.StatusBadRequest, fmt.Errorf("deleting volume %s deletes its data; confirm with --with-data",
			names.ID(volume.Namespace, volume.Name)))
		return
	}
	actor := actorOf(identityOf(r))
	batch := changeBatch{DryRun: r.URL.Query().Get("dryRun") == "true"}
	for _, ref := range request.Resources {
		cmd, err := state.NewCommand(state.KindDeleteResource, time.Time{}, actor, state.DeleteResource{
			Kind: ref.Kind, Namespace: ref.Namespace, Name: ref.Name, Owner: state.OwnerCLI,
		})
		batch.add(ref, cmd, err)
	}
	s.runOnLeader(w, r, batch)
}

func (b *changeBatch) add(ref resource.Ref, cmd state.Command, err error) {
	if err != nil {
		b.Changes = append(b.Changes, batchChange{Ref: ref, Error: err.Error()})
		return
	}
	b.Changes = append(b.Changes, batchChange{Ref: ref, Command: &cmd})
}

// putCommand turns a resource as the user wrote it into its put-resource:
// the spec read strictly and checked, a secret's value sealed and its
// digest computed. Every user's change comes from the CLI, so its owner is
// the channel cli (state.OwnerCLI).
func (s *Server) putCommand(actor string, object resource.Object) (state.Command, error) {
	if object.APIVersion != resource.APIVersion {
		return state.Command{}, fmt.Errorf("apiVersion %q is not %s", object.APIVersion, resource.APIVersion)
	}
	payload := state.PutResource{
		Kind: object.Kind, Namespace: object.Metadata.Namespace, Name: object.Metadata.Name, Owner: state.OwnerCLI,
	}
	var spec any
	switch object.Kind {
	case resource.KindNamespace:
		if err := resource.CheckNamespaceSpec(object.Spec); err != nil {
			return state.Command{}, err
		}
	case resource.KindVolume:
		volume, err := resource.DecodeVolumeSpec(object.Spec)
		if err != nil {
			return state.Command{}, err
		}
		spec = volume
	case resource.KindStack:
		// read strictly here, as the state reads the spec leniently;
		// stackspec.Check runs in the state
		stack, err := stackspec.Decode(object.Spec)
		if err != nil {
			return state.Command{}, err
		}
		spec = stack
	case resource.KindSecret:
		secret, err := resource.DecodeSecretSpec(object.Spec)
		if err != nil {
			return state.Command{}, err
		}
		identity, err := s.clusterIdentity()
		if err != nil {
			return state.Command{}, err
		}
		id := names.ID(payload.Namespace, payload.Name)
		// the value must be encrypted for this Secret: one copied from
		// elsewhere, such as the Hetzner token from cluster.yaml, would
		// otherwise reach a container
		value, err := secretvalue.OpenFor(secret.Encrypted, id, identity)
		if err != nil {
			return state.Command{}, fmt.Errorf("secret %s: %w", id, err)
		}
		sealed, err := s.sealSecret(id, value.Bytes)
		if err != nil {
			return state.Command{}, err
		}
		spec = sealed
	default:
		return state.Command{}, fmt.Errorf("no kind of resource %q", object.Kind)
	}
	if spec != nil {
		encoded, err := json.Marshal(spec)
		if err != nil {
			return state.Command{}, fmt.Errorf("encode the spec: %w", err)
		}
		payload.Spec = encoded
	}
	return state.NewCommand(state.KindPutResource, time.Time{}, actor, payload)
}

// sealSecret seals a secret's value and computes its digest: an HMAC over
// its ID and value, with a key derived from the data key. A plain hash would
// give a short password away; with the ID inside, two secrets of the same
// value have different digests.
func (s *Server) sealSecret(id string, value []byte) (state.SealedSecret, error) {
	sealed, err := s.config.Keys.Seal(value)
	if err != nil {
		return state.SealedSecret{}, fmt.Errorf("seal secret %s: %w", id, err)
	}
	key, err := s.config.Keys.Derive(secretResourcePurpose)
	if err != nil {
		return state.SealedSecret{}, fmt.Errorf("derive the digest key: %w", err)
	}
	return state.SealedSecret{Value: sealed, Digest: digest.HMAC(key, []byte(id), value)}, nil
}

// runOnLeader has the leader run a batch: here if this agent leads, else on
// the leader's /v1/internal/changes.
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

// internalChanges runs a batch a follower forwards for a user; like
// internalPropose it takes a user's commands of the allowed kinds only, and
// does not forward again: if the lead moved meanwhile, the caller asks
// again.
func (s *Server) internalChanges(w http.ResponseWriter, r *http.Request) {
	// not strict, as internalPropose: the follower may be of the next release,
	// and its commands may carry fields this one does not know
	var batch changeBatch
	if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("read the batch: %w", err))
		return
	}
	for _, each := range batch.Changes {
		if each.Command != nil && (!state.IsUserActor(each.Command.Actor) || !changeKinds[each.Command.Kind]) {
			writeError(w, http.StatusForbidden, fmt.Errorf("a batch carries only users' %v, not %s as %s",
				slices.Sorted(maps.Keys(changeKinds)), each.Command.Kind, each.Command.Actor))
			return
		}
	}
	if !s.config.Cluster.IsLeader() {
		writeError(w, http.StatusServiceUnavailable, errors.New("not the leader; try again"))
		return
	}
	writeJSON(w, http.StatusOK, s.runChanges(r.Context(), batch))
}

// runChanges runs a batch on the leader, in order, and answers what became
// of each resource. Each command goes first to a copy of the state, with
// every check of the state: a refusal answers at once, an unchanged
// resource is not proposed, and a dry run ends there, so a volume in a
// namespace that the same batch makes passes too. Only what the copy
// changed is proposed; the state checks it once more as it commits.
func (s *Server) runChanges(ctx context.Context, batch changeBatch) ChangeResult {
	answer := ChangeResult{Results: []ResourceResult{}}
	var copied *state.State
	var err error
	s.config.Cluster.Read(func(current *state.State) { copied, err = current.Copy() })
	stopped := false
	for _, each := range batch.Changes {
		result := ResourceResult{Kind: each.Ref.Kind, Namespace: each.Ref.Namespace, Name: each.Ref.Name, Outcome: NotTried}
		switch {
		case stopped:
			// stays NotTried: what follows a refusal or failure usually needs it
		case err != nil:
			result.Outcome, result.Message = Failed, err.Error()
		default:
			var index uint64
			result.Outcome, result.Message, index = s.runChange(ctx, copied, each, batch.DryRun)
			answer.Index = max(answer.Index, index)
		}
		stopped = stopped || result.Outcome == Refused || result.Outcome == Failed
		answer.Results = append(answer.Results, result)
	}
	return answer
}

// runChange runs one change: on the copy, then, unless it is unchanged or
// a dry run, through Raft. It returns the outcome, a message for a refusal
// or failure, and the committed index.
func (s *Server) runChange(ctx context.Context, copied *state.State, each batchChange, dryRun bool) (
	outcome, message string, index uint64,
) {
	if each.Command == nil {
		return Refused, each.Error, 0
	}
	// Apply takes the time from the command, so the try needs one: the
	// leader's clock. proposeHere stamps the committed command with a new
	// time. The two times differ by how long the try takes, but no check of
	// a resource reads them.
	tried := *each.Command
	tried.At = time.Now().UTC()
	// each change on the copy takes the next index: Apply sets the copy's
	// index to the one it is given
	changed, err := copied.Apply(copied.Index+1, tried)
	done := Applied
	if tried.Kind == state.KindDeleteResource {
		done = Deleted
	}
	switch {
	case err != nil:
		return Refused, reason(err), 0
	case len(changed) == 0:
		return Unchanged, "", 0
	case dryRun:
		return done, "", 0
	}
	ctx, cancel := context.WithTimeout(ctx, waitTimeout)
	defer cancel()
	index, err = s.proposeHere(ctx, *each.Command)
	var refused *state.Refused
	switch {
	case errors.As(err, &refused):
		return Refused, reason(refused), 0
	case err != nil:
		return Failed, err.Error(), 0
	}
	return done, "", index
}

// reason is why the state refused a command, without the command's kind
// that Apply puts first: the user named a resource, not a command.
func reason(err error) string {
	var refusal *state.CommandError
	if errors.As(err, &refusal) {
		return refusal.Err.Error()
	}
	return err.Error()
}
