package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/resource"
)

// Change is a resource that a command changed: its spec, its status, or
// the fact that it is gone.
type Change struct {
	Kind            string `json:"kind"`
	Namespace       string `json:"namespace,omitempty"`
	Name            string `json:"name"`
	ResourceVersion uint64 `json:"resource_version"`
	Deleted         bool   `json:"deleted,omitempty"`
}

// changed records that the command being applied changed a resource, and
// stamps the resource with the command's index.
func (s *State) changed(kind string, meta *Meta) {
	meta.ResourceVersion = s.Index
	s.changes = append(s.changes, Change{
		Kind: kind, Namespace: meta.Namespace, Name: meta.Name, ResourceVersion: s.Index,
	})
}

// ErrStale is the answer to a leader's command that was planned on a state
// in which a stack or a volume that it changes was different. The leader
// then plans again on the new state.
var ErrStale = errors.New("changed since the plan")

// checkPlanned refuses a leader's change to a resource that changed after
// the state the leader planned with (the index plannedAt). The resource may
// have changed, or been deleted and made anew under the same name; then its
// ResourceVersion is the index at which it was made. The leader plans on a
// copy of the state and commits later, so without this check it would
// overwrite what came in between.
func checkPlanned(kind, id string, stored *Meta, plannedAt uint64) error {
	switch {
	case plannedAt == 0:
		return fmt.Errorf("%s %s: the leader's command names no state it was planned with", kind, id)
	case stored.ResourceVersion > plannedAt:
		return fmt.Errorf("%s %s at %d, planned at %d: %w", kind, id, stored.ResourceVersion, plannedAt, ErrStale)
	}
	return nil
}

// deleted records that the command being applied removed a resource.
func (s *State) deleted(kind string, meta Meta) {
	s.changes = append(s.changes, Change{
		Kind: kind, Namespace: meta.Namespace, Name: meta.Name, ResourceVersion: s.Index, Deleted: true,
	})
}

// PutResource creates or changes a resource. A spec equal to the stored
// one changes nothing, neither the generation nor the resource version, so
// applying the same files again restarts nothing.
type PutResource struct {
	// Kind is the resource's (resource.KindVolume), not the command's.
	Kind string `json:"kind"`
	// Namespace is empty for a namespace, which lives in none.
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	Owner     string `json:"owner"` // the channel: OwnerCLI so far
	// Spec is the kind's spec: none for a namespace, otherwise
	// resource.VolumeSpec, EncryptedSecret or stackspec.Spec.
	Spec json.RawMessage `json:"spec,omitempty"`
	// Create refuses a resource that exists, as kubectl create does
	// (clowder create secret). It needs no gate of its own in CheckKnown:
	// change-resources itself comes with 0.2.0, so every agent that reads
	// the command reads this field too.
	Create bool `json:"create,omitempty"`
}

// DeleteResource removes a resource that nothing uses.
type DeleteResource struct {
	Kind      string `json:"kind"` // the resource's
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	Owner     string `json:"owner"`
}

// resourceKind says how the state keeps one kind of resource, by its ID
// (resourceID). put and remove only check. They return the change as a
// function that the handler runs once every check has passed, so a refused
// command leaves no trace (docs/developing/cluster.md, Cluster state).
type resourceKind struct {
	// find returns the resource's Meta, or nil if there is none.
	find func(s *State, id string) *Meta
	// put and remove only read the state and return their change, as a
	// handler does, so a refused command leaves no trace.
	//
	// put first decodes the spec and compares it with the stored one; for
	// an equal spec it returns errUnchanged. Then it checks the spec against
	// the state and returns the change that stores it. The change returns
	// the stored Meta, so that the handler can count the generation.
	put func(s *State, id string, payload PutResource) (store func() *Meta, err error)
	// remove checks that nothing uses the resource, then returns the change
	// that removes it.
	remove func(s *State, id string) (func(), error)
}

var resourceKinds = map[string]resourceKind{
	resource.KindClusterConfiguration: {find: findConfig, put: putConfig, remove: removeConfig},
	resource.KindNamespace:            {find: findNamespace, put: putNamespace, remove: removeNamespace},
	resource.KindVolume:               {find: findVolume, put: putVolumeResource, remove: removeVolumeResource},
	resource.KindSecret:               {find: findSecret, put: putSecret, remove: removeSecret},
	resource.KindStack:                {find: findStack, put: putStack, remove: removeStack},
}

// ChangeResources puts and deletes resources in order, all of them or none.
// Each change is checked against the state with the changes before it
// applied. So a Secret placed before the configuration that names it, or a
// namespace placed before its stacks, goes in together with them. Every
// apply and delete of a user comes in this way (api's runChanges).
type ChangeResources struct {
	Changes []ResourceChange `json:"changes"`
}

// ResourceChange is one put or one delete.
type ResourceChange struct {
	Put    *PutResource    `json:"put,omitempty"`
	Delete *DeleteResource `json:"delete,omitempty"`
}

// ChangeError is a change-resources command that was refused at one of its
// changes, given by index. None of its changes went in.
type ChangeError struct {
	Index int
	Err   error
}

func (e *ChangeError) Error() string { return fmt.Sprintf("change %d: %v", e.Index+1, e.Err) }

func (e *ChangeError) Unwrap() error { return e.Err }

// applyChangeResources applies the changes to a clone of the state, one
// after the other, because each needs the changes before it. The function
// it returns puts the clone in place. This is the one handler that works on
// a clone, because what a change checks depends on the changes before it.
// The clone shares the specs and the encrypted values (State.Clone), so a
// batch costs about as much as the resources' statuses, not the secrets. A
// change that changes nothing does not count, and a batch of only such
// changes is unchanged.
func applyChangeResources(s *State, cmd Command, payload ChangeResources) (func(), error) {
	if len(payload.Changes) == 0 {
		return nil, errors.New("no changes")
	}
	copied := s.Clone()
	for index, each := range payload.Changes {
		var change func()
		var err error
		switch {
		case (each.Put == nil) == (each.Delete == nil):
			err = errors.New("a change is one put or one delete")
		case each.Put != nil:
			change, err = applyPutResource(copied, *each.Put)
		default:
			change, err = applyDeleteResource(copied, *each.Delete)
		}
		switch {
		case errors.Is(err, errUnchanged):
			continue
		case err != nil:
			return nil, &ChangeError{Index: index, Err: err}
		}
		change()
	}
	if len(copied.changes) == 0 {
		return nil, errUnchanged
	}
	return func() {
		// Clone leaves the audit out, and Apply adds this command's entry
		// to the state's own
		audit := s.Audit
		*s = *copied
		s.Audit = audit
	}, nil
}

// applyPutResource checks in a fixed order: which channel owns the
// resource, whether anything changes, and only then everything else. It
// does not check the actor: the cluster has no users, so every CLI
// certificate may change every resource, and the API decides who may send
// change-resources at all. This way a channel learns nothing about another
// channel's spec, only that the resource belongs to another channel. And an
// unchanged resource passes even if the state has changed since in a way
// that would make the same spec fail the other checks.
func applyPutResource(s *State, payload PutResource) (func(), error) {
	kind, ok := resourceKinds[payload.Kind]
	if !ok {
		return nil, fmt.Errorf("no kind of resource %q", payload.Kind)
	}
	id, err := s.resourceID(payload.Kind, payload.Namespace, payload.Name)
	if err != nil {
		return nil, err
	}
	if payload.Owner == "" {
		return nil, fmt.Errorf("%s %s needs an owner", payload.Kind, id)
	}
	if payload.Namespace == names.System && (payload.Kind == resource.KindStack || payload.Kind == resource.KindVolume) {
		return nil, fmt.Errorf("namespace %s holds the cluster's Secrets only, no %ss", names.System, payload.Kind)
	}
	stored := kind.find(s, id)
	if payload.Create && stored != nil {
		return nil, fmt.Errorf("%s %s already exists", payload.Kind, id)
	}
	if err := checkOwner(stored, payload.Kind, id, payload.Owner); err != nil {
		return nil, err
	}
	store, err := kind.put(s, id, payload)
	if err != nil {
		return nil, err
	}
	return func() {
		meta := store()
		meta.Generation++
		s.changed(payload.Kind, meta)
	}, nil
}

func applyDeleteResource(s *State, payload DeleteResource) (func(), error) {
	kind, ok := resourceKinds[payload.Kind]
	if !ok {
		return nil, fmt.Errorf("no kind of resource %q", payload.Kind)
	}
	id, err := s.resourceID(payload.Kind, payload.Namespace, payload.Name)
	if err != nil {
		return nil, err
	}
	stored := kind.find(s, id)
	if stored == nil {
		return nil, fmt.Errorf("no %s %s", payload.Kind, id)
	}
	if err := checkOwner(stored, payload.Kind, id, payload.Owner); err != nil {
		return nil, err
	}
	meta := *stored
	remove, err := kind.remove(s, id)
	if err != nil {
		return nil, err
	}
	return func() {
		remove()
		s.deleted(payload.Kind, meta)
	}, nil
}

// resourceID returns a resource's key in its map: a namespace's name, or
// else names.ID. It refuses names that are not valid and a namespace that
// the cluster does not have.
func (s *State) resourceID(kind, namespace, name string) (string, error) {
	if !resource.Namespaced(kind) {
		switch {
		case namespace != "":
			return "", fmt.Errorf("a %s lives in no namespace", kind)
		case kind == resource.KindNamespace && name != names.System && !names.ValidNamespace(name):
			return "", fmt.Errorf("namespace name %q: %s", name, names.NamespaceRule)
		}
		return name, nil
	}
	if !names.Valid(name) {
		return "", fmt.Errorf("%s name %q: %s", kind, name, names.Rule)
	}
	if _, ok := s.Namespaces[namespace]; !ok {
		return "", fmt.Errorf("no namespace %q", namespace)
	}
	return names.ID(namespace, name), nil
}

// checkOwner refuses a channel other than the resource's owner. A resource
// without an owner (names.Default) belongs to every channel.
func checkOwner(stored *Meta, kind, id, owner string) error {
	if stored != nil && stored.Owner != "" && stored.Owner != owner {
		return fmt.Errorf("%s %s belongs to %s, not to %s", kind, id, stored.Owner, owner)
	}
	return nil
}

// decodeSpec reads a spec. Unknown fields are ignored, as in every payload:
// a command of the next release may carry fields this one does not know.
func decodeSpec[Spec any](kind, id string, raw json.RawMessage) (Spec, error) {
	var spec Spec
	if len(raw) == 0 {
		return spec, nil
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		return spec, fmt.Errorf("%s %s: read the spec: %w", kind, id, err)
	}
	return spec, nil
}

func findNamespace(s *State, id string) *Meta {
	if namespace, ok := s.Namespaces[id]; ok {
		return &namespace.Meta
	}
	return nil
}

// putNamespace makes a namespace or changes its spec. Its owner owns only
// the namespace: any channel may put resources into it. names.System holds
// no stacks, so it takes no image pull secrets.
func putNamespace(s *State, id string, payload PutResource) (func() *Meta, error) {
	spec, err := decodeSpec[resource.NamespaceSpec](payload.Kind, id, payload.Spec)
	if err != nil {
		return nil, err
	}
	stored, exists := s.Namespaces[id]
	if exists && sameJSON(stored.Spec, spec) {
		return nil, errUnchanged
	}
	if err := spec.Check(); err != nil {
		return nil, fmt.Errorf("namespace %s: %w", id, err)
	}
	if id == names.System {
		return nil, fmt.Errorf("namespace %s holds no stacks, so it takes no image pull secrets", id)
	}
	return func() *Meta {
		if !exists {
			stored = &Namespace{Meta: Meta{Name: id, Owner: payload.Owner}}
			s.Namespaces[id] = stored
		}
		stored.Spec = spec
		return &stored.Meta
	}, nil
}

// removeNamespace refuses names.Default, names.System and a namespace that
// holds resources. Nothing cascades; the user empties the namespace first.
// Retired copies of its volumes do not count. They stay in the state until
// their hosts have removed them, even if a namespace of the same name is
// made again.
func removeNamespace(s *State, id string) (func(), error) {
	if id == names.Default || id == names.System {
		return nil, fmt.Errorf("namespace %s belongs to the cluster", id)
	}
	inNamespace := func(resourceID string) bool {
		namespace, _, _ := names.SplitID(resourceID)
		return namespace == id
	}
	for _, held := range []struct {
		kind string
		ids  []string
	}{
		{resource.KindStack, slices.Sorted(maps.Keys(s.Stacks))},
		{resource.KindVolume, slices.Sorted(maps.Keys(s.Volumes))},
		{resource.KindSecret, slices.Sorted(maps.Keys(s.Secrets))},
	} {
		if index := slices.IndexFunc(held.ids, inNamespace); index >= 0 {
			return nil, fmt.Errorf("namespace %s still holds %s %s; delete what it holds first",
				id, held.kind, held.ids[index])
		}
	}
	return func() { delete(s.Namespaces, id) }, nil
}

// sameJSON tells whether two values encode to the same JSON. The state
// compares what it stores this way, not field by field. An agent that read
// its state from a snapshot (JSON, where an empty list and no list read
// back the same) must decide like one that applied the whole log.
func sameJSON(a, b any) bool {
	encodedA, errA := json.Marshal(a)
	encodedB, errB := json.Marshal(b)
	return errA == nil && errB == nil && bytes.Equal(encodedA, encodedB)
}

// stackNaming returns the first stack, by ID, that names the volume or
// secret name of a namespace, or "" if none does. named returns what a stack
// names (its volumes or its secrets). The stack with the ID except is left
// out, because it is the one being put.
func (s *State) stackNaming(namespace, name, except string, named func(stack *Stack) []string) string {
	for _, id := range slices.Sorted(maps.Keys(s.Stacks)) {
		stack := s.Stacks[id]
		if id != except && stack.Namespace == namespace && slices.Contains(named(stack), name) {
			return id
		}
	}
	return ""
}
