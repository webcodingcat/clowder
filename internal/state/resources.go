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

// Change is a resource a command changed: its spec, its status, or that it
// is gone.
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
// in which a stack or a volume it changes was another: the leader plans
// again on the new one.
var ErrStale = errors.New("changed since the plan")

// checkPlanned refuses a leader's change to a resource that changed after
// the state the leader planned with (the index plannedAt): changed, or
// deleted and made anew under the same name, whose ResourceVersion is then
// the index of its making. The leader plans on a copy of the state and
// commits after, so without this check it would write over what came in
// between.
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
// one changes nothing: not the generation, not the resource version, so
// applying the same files again restarts nothing.
type PutResource struct {
	// Kind is the resource's (resource.KindVolume), not the command's.
	Kind string `json:"kind"`
	// Namespace is empty for a namespace, which lives in none.
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	Owner     string `json:"owner"` // the channel: OwnerCLI so far
	// Spec is the kind's: none for a namespace, resource.VolumeSpec,
	// SealedSecret, stackspec.Spec.
	Spec json.RawMessage `json:"spec,omitempty"`
}

// DeleteResource removes a resource that nothing uses.
type DeleteResource struct {
	Kind      string `json:"kind"` // the resource's
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	Owner     string `json:"owner"`
}

// resourceKind is how the state keeps one kind of resource, by its ID
// (resourceID). put and remove only check; they return the change as a
// function that the handler runs once every check has passed, so a refused
// command leaves no trace (docs/developing/cluster.md, Cluster state).
type resourceKind struct {
	// find returns the resource's Meta, or nil if there is none.
	find func(s *State, id string) *Meta
	// put and remove only read the state and return their change, as a
	// handler does, so a refused command leaves no trace.
	//
	// put decodes the spec and compares it with the stored one first: for
	// an equal one it returns errUnchanged. Then it checks the spec against
	// the state and returns the change that stores it; the change returns
	// the stored Meta, for the handler to count the generation.
	put func(s *State, id string, payload PutResource) (store func() *Meta, err error)
	// remove checks that nothing uses the resource, then returns the change
	// that removes it.
	remove func(s *State, id string) (func(), error)
}

var resourceKinds = map[string]resourceKind{
	resource.KindNamespace: {find: findNamespace, put: putNamespace, remove: removeNamespace},
	resource.KindVolume:    {find: findVolume, put: putVolumeResource, remove: removeVolumeResource},
	resource.KindSecret:    {find: findSecret, put: putSecret, remove: removeSecret},
	resource.KindStack:     {find: findStack, put: putStack, remove: removeStack},
}

// applyPutResource checks in a fixed order: whether the actor may put the
// resource, which channel owns it, whether anything changes, and only then
// everything else. This way a channel learns nothing about another
// channel's spec, only that the resource belongs to another channel. And an
// unchanged resource passes even if the state has changed since in a way
// that would make the same spec fail the other checks.
func applyPutResource(s *State, cmd Command, payload PutResource) (func(), error) {
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
	if err := checkOwner(kind.find(s, id), payload.Kind, id, payload.Owner); err != nil {
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

func applyDeleteResource(s *State, cmd Command, payload DeleteResource) (func(), error) {
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

// resourceID is a resource's key in its map: a namespace's name, else
// names.ID. It refuses names that are not valid and a namespace the cluster
// does not have.
func (s *State) resourceID(kind, namespace, name string) (string, error) {
	if !resource.Namespaced(kind) {
		switch {
		case namespace != "":
			return "", fmt.Errorf("a %s lives in no namespace", kind)
		case !names.ValidNamespace(name):
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
// without one (names.Default) is every channel's.
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

// putNamespace makes a namespace; it has no spec, so an existing one is
// unchanged. Its owner owns only the namespace: any channel may put
// resources into it.
func putNamespace(s *State, id string, payload PutResource) (func() *Meta, error) {
	spec, err := decodeSpec[map[string]json.RawMessage](payload.Kind, id, payload.Spec)
	if err != nil {
		return nil, err
	}
	if len(spec) > 0 {
		return nil, fmt.Errorf("namespace %s: a namespace has no spec", id)
	}
	if _, ok := s.Namespaces[id]; ok {
		return nil, errUnchanged
	}
	return func() *Meta {
		namespace := &Namespace{Meta: Meta{Name: id, Owner: payload.Owner}}
		s.Namespaces[id] = namespace
		return &namespace.Meta
	}, nil
}

// removeNamespace refuses names.Default and a namespace that holds
// resources: nothing cascades, the user empties it first. Retired copies
// of its volumes do not count; they stay in the state until their hosts
// removed them, also for a namespace made again under the same name.
func removeNamespace(s *State, id string) (func(), error) {
	if id == names.Default {
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

// sameJSON tells whether two values encode alike. The state compares what
// it stores this way, not field by field: an agent that read its state from
// a snapshot (JSON, where an empty list and none read back alike) must
// decide as one that applied the whole log.
func sameJSON(a, b any) bool {
	encodedA, errA := json.Marshal(a)
	encodedB, errB := json.Marshal(b)
	return errA == nil && errB == nil && bytes.Equal(encodedA, encodedB)
}

// stackNaming returns the first stack, by ID, that names the volume or
// secret name of a namespace, or "" if none does. named returns what a stack
// names (its volumes or its secrets). The stack with the ID except is left
// out: it is the one being put.
func (s *State) stackNaming(namespace, name, except string, named func(stack *Stack) []string) string {
	for _, id := range slices.Sorted(maps.Keys(s.Stacks)) {
		stack := s.Stacks[id]
		if id != except && stack.Namespace == namespace && slices.Contains(named(stack), name) {
			return id
		}
	}
	return ""
}
