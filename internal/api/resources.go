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

// ResourceList is the answer to GET /v1/resources/{kind}: the resources
// sorted by namespace and name, and the state's index they were read at.
type ResourceList struct {
	Index uint64            `json:"index"`
	Items []resource.Object `json:"items"`
}

// VolumeStatus is a volume's status as the API shows it.
type VolumeStatus struct {
	Status         string       `json:"status"`
	DataGeneration int64        `json:"dataGeneration"`
	Copies         []VolumeCopy `json:"copies,omitempty"`
}

// VolumeCopy is one copy of a volume.
type VolumeCopy struct {
	Host       string `json:"host"`
	Class      string `json:"class"`
	Tiebreaker bool   `json:"tiebreaker,omitempty"`
}

// SecretStatus is a secret's status: its digest, never its value.
type SecretStatus struct {
	Digest string `json:"digest"`
}

// StackStatus is a stack's status as the API shows it.
type StackStatus struct {
	Status string `json:"status"`
	Host   string `json:"host,omitempty"` // empty: not placed yet
	// Priority is the stack's as the cluster resolves it now; Default tells
	// that the spec names none and cluster.yaml's default applies.
	Priority int  `json:"priority"`
	Default  bool `json:"default,omitempty"`
	// PushedOut are the services stopped for others of higher priority.
	PushedOut []string `json:"pushedOut,omitempty"`
	// WaitReason is why the stack or a volume it names finds no place.
	WaitReason string `json:"waitReason,omitempty"`
	// AppliedSecrets are the digests of the secrets its containers came up
	// with: one that differs from the secret's own has not reached them yet.
	AppliedSecrets map[string]string `json:"appliedSecrets,omitempty"`
	// Restarted is when each service was last restarted (stack restart).
	Restarted map[string]time.Time `json:"restarted,omitempty"`
}

// secretResourcePurpose names the key the digests of Secret resources
// take, apart from the key of the containers' labels: neither digest tells
// anything about the other.
const secretResourcePurpose = "clowder secret resource digest"

// changeBatch is an apply or a delete as the agent that took it hands it to
// the leader: each resource's command in order, or why it has none. The
// leader decides on its own state; a follower's may be behind and would
// answer unchanged or refused for what has changed since.
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
	s.decide(w, r, batch)
}

// deleteResources deletes resources, in the order sent.
func (s *Server) deleteResources(w http.ResponseWriter, r *http.Request) {
	var request DeleteRequest
	if err := decodeStrict(r.Body, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
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
	s.decide(w, r, batch)
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
		sealed, err := s.sealSecret(names.ID(payload.Namespace, payload.Name), secret.Value)
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
			return state.Command{}, err
		}
		payload.Spec = encoded
	}
	return state.NewCommand(state.KindPutResource, time.Time{}, actor, payload)
}

// sealSecret seals a secret's value and computes its digest: an HMAC over
// its ID and value, with a key derived from the data key. A plain hash would
// give a short password away; with the ID inside, two secrets of the same
// value have different digests.
func (s *Server) sealSecret(id, value string) (state.SealedSecret, error) {
	sealed, err := s.config.Keys.Seal([]byte(value))
	if err != nil {
		return state.SealedSecret{}, fmt.Errorf("seal secret %s: %w", id, err)
	}
	key, err := s.config.Keys.Derive(secretResourcePurpose)
	if err != nil {
		return state.SealedSecret{}, err
	}
	return state.SealedSecret{Value: sealed, Digest: digest.HMAC(key, []byte(id), []byte(value))}, nil
}

// decide has the leader run a batch: here if this agent leads, else on the
// leader's /v1/internal/changes.
func (s *Server) decide(w http.ResponseWriter, r *http.Request, batch changeBatch) {
	var answer ChangeResult
	if s.config.Cluster.IsLeader() {
		answer = s.runChanges(r.Context(), batch)
	} else {
		var err error
		if answer, err = s.forwardChanges(r.Context(), batch); err != nil {
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
func (s *Server) forwardChanges(ctx context.Context, batch changeBatch) (ChangeResult, error) {
	leader, ok := s.config.Cluster.Leader()
	if !ok {
		return ChangeResult{}, errors.New("the cluster has no leader right now")
	}
	body, err := json.Marshal(batch)
	if err != nil {
		return ChangeResult{}, err
	}
	url := "https://" + s.config.APIAddress(leader) + "/v1/internal/changes"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return ChangeResult{}, err
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
	var batch changeBatch
	if err := decodeStrict(r.Body, &batch); err != nil {
		writeError(w, http.StatusBadRequest, err)
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
func (s *Server) runChange(ctx context.Context, copied *state.State, each batchChange, dryRun bool) (string, string, uint64) {
	if each.Command == nil {
		return Refused, each.Error, 0
	}
	tried := *each.Command
	tried.At = time.Now().UTC() // the leader's clock, as proposeHere stamps it
	// Apply moves the copy's index, so the next change gets the one after
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
	index, err := s.proposeHere(ctx, *each.Command)
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

// listResources lists the resources of a kind: of one namespace
// (?namespace=) or of all (?allNamespaces=true); there is no default, so a
// controller never reads the wrong one by accident.
func (s *Server) listResources(w http.ResponseWriter, r *http.Request) {
	kind, ok := resource.KindOf(r.PathValue("kind"))
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Errorf("no kind of resource %q", r.PathValue("kind")))
		return
	}
	namespace := r.URL.Query().Get("namespace")
	all := r.URL.Query().Get("allNamespaces") == "true"
	switch {
	case !resource.Namespaced(kind) && (namespace != "" || all):
		writeError(w, http.StatusBadRequest, fmt.Errorf("a %s lives in no namespace", kind))
		return
	case resource.Namespaced(kind) && (namespace == "") == !all:
		writeError(w, http.StatusBadRequest, errors.New("name either namespace= or allNamespaces=true"))
		return
	}
	if err := state.Authorize(actorOf(identityOf(r)), state.VerbRead, kind, namespace); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	list := ResourceList{Items: []resource.Object{}}
	s.config.Cluster.Read(func(current *state.State) {
		list.Index = current.Index
		for _, object := range objects(current, kind) {
			if all || object.Metadata.Namespace == namespace {
				list.Items = append(list.Items, object)
			}
		}
	})
	writeJSON(w, http.StatusOK, list)
}

// getResource answers one resource of a namespace:
// /v1/resources/{kind}/{namespace}/{name}.
func (s *Server) getResource(w http.ResponseWriter, r *http.Request) {
	kind, ok := resource.KindOf(r.PathValue("kind"))
	if !ok || !resource.Namespaced(kind) {
		writeError(w, http.StatusNotFound, fmt.Errorf("no resource at %s", r.URL.Path))
		return
	}
	s.answerResource(w, r, resource.Ref{Kind: kind, Namespace: r.PathValue("namespace"), Name: r.PathValue("name")})
}

// getNamespace answers one namespace, which lives in none:
// /v1/resources/namespace/{name}.
func (s *Server) getNamespace(w http.ResponseWriter, r *http.Request) {
	s.answerResource(w, r, resource.Ref{Kind: resource.KindNamespace, Name: r.PathValue("name")})
}

func (s *Server) answerResource(w http.ResponseWriter, r *http.Request, ref resource.Ref) {
	if err := state.Authorize(actorOf(identityOf(r)), state.VerbRead, ref.Kind, ref.Namespace); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	var found *resource.Object
	s.config.Cluster.Read(func(current *state.State) {
		for _, object := range objects(current, ref.Kind) {
			if object.Ref() == ref {
				found = &object
				return
			}
		}
	})
	if found == nil {
		writeError(w, http.StatusNotFound, fmt.Errorf("no %s", ref))
		return
	}
	writeJSON(w, http.StatusOK, found)
}

// objects are the resources of a kind as the API shows them, sorted by
// namespace and name. A secret shows its digest, never its value.
func objects(current *state.State, kind string) []resource.Object {
	var shown []resource.Object
	add := func(meta state.Meta, spec, status any) {
		object := resource.Object{
			APIVersion: resource.APIVersion, Kind: kind,
			Metadata: resource.Metadata{
				Namespace: meta.Namespace, Name: meta.Name, Owner: meta.Owner,
				Generation: meta.Generation, ResourceVersion: meta.ResourceVersion,
			},
		}
		// the API's own types: they always encode
		if spec != nil {
			object.Spec, _ = json.Marshal(spec)
		}
		if status != nil {
			object.Status, _ = json.Marshal(status)
		}
		shown = append(shown, object)
	}
	switch kind {
	case resource.KindNamespace:
		for _, name := range slices.Sorted(maps.Keys(current.Namespaces)) {
			add(current.Namespaces[name].Meta, nil, nil)
		}
	case resource.KindVolume:
		for _, id := range slices.Sorted(maps.Keys(current.Volumes)) {
			volume := current.Volumes[id]
			add(volume.Meta, volume.Spec, volumeStatus(current, volume))
		}
	case resource.KindSecret:
		for _, id := range slices.Sorted(maps.Keys(current.Secrets)) {
			secret := current.Secrets[id]
			add(secret.Meta, nil, SecretStatus{Digest: secret.Spec.Digest})
		}
	case resource.KindStack:
		for _, id := range slices.Sorted(maps.Keys(current.Stacks)) {
			stack := current.Stacks[id]
			add(stack.Meta, stack.Spec, stackStatus(current, stack))
		}
	}
	return shown
}

func stackStatus(current *state.State, stack *state.Stack) StackStatus {
	status := StackStatus{
		Status: string(stack.Status), PushedOut: stack.PushedOut, WaitReason: stack.WaitReason,
		AppliedSecrets: stack.AppliedSecrets, Restarted: stack.Restarted, Default: stack.Spec.Priority == "",
	}
	if stack.Host != nil {
		if host, ok := current.Hosts[*stack.Host]; ok {
			status.Host = host.Name
		}
	}
	// the state checked that it resolves, at the apply and at every
	// cluster apply since
	written, _ := stackspec.ParsePriority(stack.Spec.Priority)
	status.Priority, _ = current.Config.Spec.WithDefaults().Priorities.Resolve(written)
	return status
}

func volumeStatus(current *state.State, volume *state.Volume) VolumeStatus {
	status := VolumeStatus{Status: string(volume.Status), DataGeneration: volume.DataGeneration}
	for _, placement := range volume.Placements {
		host := strconv.Itoa(int(placement.Host))
		if known, ok := current.Hosts[placement.Host]; ok {
			host = known.Name
		}
		status.Copies = append(status.Copies, VolumeCopy{
			Host: host, Class: placement.Class, Tiebreaker: placement.Tiebreaker,
		})
	}
	return status
}
