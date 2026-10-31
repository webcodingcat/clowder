package state

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

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

// deleted records that the command being applied removed a resource.
func (s *State) deleted(kind string, meta Meta) {
	s.changes = append(s.changes, Change{
		Kind: kind, Namespace: meta.Namespace, Name: meta.Name, ResourceVersion: s.Index, Deleted: true,
	})
}

// The verbs Authorize knows so far; restart and read come with the commands
// that need them.
const (
	VerbPut    = "put"
	VerbDelete = "delete"
)

// Authorize tells whether an actor may do a verb to the resources of a kind
// in a namespace. It decides on the leader, in the state, so no agent that
// took a request decides for it; the API will ask it too, to answer early.
// Until users get permissions every user may do everything.
func Authorize(actor, verb, kind, namespace string) error {
	_, _, _, _ = actor, verb, kind, namespace // read once users get permissions
	return nil
}

// PutResource creates or changes a resource. A spec equal to the stored
// one changes nothing: not the generation, not the resource version, not
// ResourcesRevision, so applying the same files again restarts nothing.
type PutResource struct {
	// Kind is the resource's (resource.KindVolume), not the command's.
	Kind string `json:"kind"`
	// Namespace is empty for a namespace, which lives in none.
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	Owner     string `json:"owner"` // the channel: OwnerCLI or GitOpsOwner(source)
	// Spec is the kind's: none for a namespace, resource.VolumeSpec,
	// SealedSecret.
	Spec json.RawMessage `json:"spec,omitempty"`
}

// DeleteResource removes a resource that nothing uses.
type DeleteResource struct {
	Kind string `json:"kind"` // the resource's

	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	Owner     string `json:"owner"`
}

// resourceKind is how the state keeps one kind of resource, by its ID
// (resourceID).
type resourceKind struct {
	// find returns the resource's Meta, or nil if there is none.
	find func(s *State, id string) *Meta
	// put decodes the spec and compares it with the stored one first: for
	// an equal one it returns nil and changes nothing. Then it checks the
	// spec against the state and stores it, returning the stored Meta.
	put func(s *State, id string, payload PutResource) (*Meta, error)
	// remove checks that nothing uses the resource, then removes it.
	remove func(s *State, id string) error
}

var resourceKinds = map[string]resourceKind{
	resource.KindNamespace: {find: findNamespace, put: putNamespace, remove: removeNamespace},
	resource.KindVolume:    {find: findVolume, put: putVolumeResource, remove: removeVolumeResource},
	resource.KindSecret:    {find: findSecret, put: putSecret, remove: removeSecret},
}

// applyPutResource checks in a fixed order: who may, whose the resource is,
// whether anything changes, and only then the rest; so a channel learns
// nothing of another's resources, and an unchanged resource passes even
// where its state has moved on since.
func applyPutResource(s *State, cmd Command, payload PutResource) error {
	kind, ok := resourceKinds[payload.Kind]
	if !ok {
		return fmt.Errorf("no kind of resource %q", payload.Kind)
	}
	if err := Authorize(cmd.Actor, VerbPut, payload.Kind, payload.Namespace); err != nil {
		return err
	}
	id, err := s.resourceID(payload.Kind, payload.Namespace, payload.Name)
	if err != nil {
		return err
	}
	if payload.Owner == "" {
		return fmt.Errorf("%s %s needs an owner", payload.Kind, id)
	}
	if err := checkOwner(kind.find(s, id), payload.Kind, id, payload.Owner); err != nil {
		return err
	}
	meta, err := kind.put(s, id, payload)
	switch {
	case err != nil:
		return err
	case meta == nil:
		return errUnchanged
	}
	meta.Generation++
	s.ResourcesRevision++
	s.changed(payload.Kind, meta)
	return nil
}

func applyDeleteResource(s *State, cmd Command, payload DeleteResource) error {
	kind, ok := resourceKinds[payload.Kind]
	if !ok {
		return fmt.Errorf("no kind of resource %q", payload.Kind)
	}
	if err := Authorize(cmd.Actor, VerbDelete, payload.Kind, payload.Namespace); err != nil {
		return err
	}
	id, err := s.resourceID(payload.Kind, payload.Namespace, payload.Name)
	if err != nil {
		return err
	}
	stored := kind.find(s, id)
	if stored == nil {
		return fmt.Errorf("no %s %s", payload.Kind, id)
	}
	if err := checkOwner(stored, payload.Kind, id, payload.Owner); err != nil {
		return err
	}
	meta := *stored
	if err := kind.remove(s, id); err != nil {
		return err
	}
	s.ResourcesRevision++
	s.deleted(payload.Kind, meta)
	return nil
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
func putNamespace(s *State, id string, payload PutResource) (*Meta, error) {
	if _, ok := s.Namespaces[id]; ok {
		return nil, nil
	}
	spec, err := decodeSpec[map[string]json.RawMessage](payload.Kind, id, payload.Spec)
	if err != nil {
		return nil, err
	}
	if len(spec) > 0 {
		return nil, fmt.Errorf("namespace %s: a namespace has no spec", id)
	}
	namespace := &Namespace{Meta: Meta{Name: id, Owner: payload.Owner}}
	s.Namespaces[id] = namespace
	return &namespace.Meta, nil
}

// removeNamespace refuses names.Default and a namespace that holds
// resources: nothing cascades, the user empties it first. Retired copies
// of its volumes do not count; they stay in the state until their hosts
// removed them, also for a namespace made again under the same name.
func removeNamespace(s *State, id string) error {
	if id == names.Default {
		return fmt.Errorf("namespace %s belongs to the cluster", id)
	}
	// every ID starts with its namespace: a stack's, a volume's of either
	// shape, a secret's
	inNamespace := func(resourceID string) bool {
		namespace, _, _ := strings.Cut(resourceID, "/")
		return namespace == id
	}
	for _, held := range []struct {
		kind string
		ids  []string
	}{
		{"stack", slices.Sorted(maps.Keys(s.Stacks))},
		{"volume", slices.Sorted(maps.Keys(s.Volumes))},
		{"secret", slices.Sorted(maps.Keys(s.Secrets))},
	} {
		if index := slices.IndexFunc(held.ids, inNamespace); index >= 0 {
			return fmt.Errorf("namespace %s still holds %s %s; delete what it holds first", id, held.kind, held.ids[index])
		}
	}
	delete(s.Namespaces, id)
	return nil
}

func findVolume(s *State, id string) *Meta {
	if volume, ok := s.Volumes[id]; ok {
		return &volume.Meta
	}
	return nil
}

// putVolumeResource makes a volume. It is placed once a stack names it,
// on that stack's host; until then it waits (VolumePending).
func putVolumeResource(s *State, id string, payload PutResource) (*Meta, error) {
	spec, err := decodeSpec[resource.VolumeSpec](payload.Kind, id, payload.Spec)
	if err != nil {
		return nil, err
	}
	if stored, ok := s.Volumes[id]; ok {
		if stored.Spec == spec {
			return nil, nil
		}
		return nil, fmt.Errorf("volume %s %w", id, stored.Spec.CheckUnchanged(spec))
	}
	if err := spec.Check(); err != nil {
		return nil, fmt.Errorf("volume %s: %w", id, err)
	}
	if _, ok := s.Config.Spec.StorageClasses[spec.Class]; !ok {
		return nil, fmt.Errorf("volume %s: the cluster has no storage class %q", id, spec.Class)
	}
	generation := s.nextDataGeneration(id)
	if err := s.checkFreeLV(id, generation); err != nil {
		return nil, err
	}
	volume := &Volume{
		Meta: Meta{Namespace: payload.Namespace, Name: payload.Name, Owner: payload.Owner},
		ID:   id, Spec: spec, DataGeneration: generation, Status: VolumePending,
	}
	s.Volumes[id] = volume
	return &volume.Meta, nil
}

// nextDataGeneration is a new volume's: one above the highest retired copy
// of a deleted volume of the same ID, so its LV cannot be taken for one of
// theirs, whose hosts may not have removed them yet.
func (s *State) nextDataGeneration(id string) int64 {
	generation := int64(1)
	for _, retired := range s.Retired {
		if retired.Volume == id {
			generation = max(generation, retired.DataGeneration+1)
		}
	}
	return generation
}

// checkFreeLV refuses an LV name that a volume or a retired copy has
// already: the name holds only 32 bits of a hash of the ID.
func (s *State) checkFreeLV(id string, generation int64) error {
	if generation > names.MaxDataGeneration {
		return fmt.Errorf("volume %s: data generation %d is beyond %d", id, generation, names.MaxDataGeneration)
	}
	lv := names.VolumeName(id, generation)
	for _, otherID := range slices.Sorted(maps.Keys(s.Volumes)) {
		other := s.Volumes[otherID]
		if names.VolumeName(other.ID, other.DataGeneration) == lv {
			return fmt.Errorf("volume %s would have the LV of volume %s; choose another name", id, other.ID)
		}
	}
	for _, retired := range s.Retired {
		if names.VolumeName(retired.Volume, retired.DataGeneration) == lv {
			return fmt.Errorf("volume %s would have the LV of a removed copy of %s; choose another name",
				id, retired.Volume)
		}
	}
	return nil
}

// removeVolumeResource retires the volume's copies: their hosts remove them.
func removeVolumeResource(s *State, id string) error {
	if err := s.checkUnlocked(volumeLock(id), ""); err != nil {
		return err
	}
	s.retire(s.Volumes[id], nil)
	delete(s.Volumes, id)
	return nil
}

func findSecret(s *State, id string) *Meta {
	if secret, ok := s.Secrets[id]; ok {
		return &secret.Meta
	}
	return nil
}

// putSecret stores a secret's sealed value. The limits are on the values in
// clear (ClearSize): per value, per namespace and in the cluster, as every
// apply writes them into the Raft log and every snapshot holds them.
func putSecret(s *State, id string, payload PutResource) (*Meta, error) {
	spec, err := decodeSpec[SealedSecret](payload.Kind, id, payload.Spec)
	if err != nil {
		return nil, err
	}
	stored, exists := s.Secrets[id]
	if exists && stored.Spec.Digest == spec.Digest {
		return nil, nil
	}
	size := ClearSize(spec.Value)
	switch {
	case spec.Digest == "":
		return nil, fmt.Errorf("secret %s has no digest", id)
	case size < 0:
		return nil, fmt.Errorf("secret %s: the value is not sealed", id)
	case size == 0:
		return nil, fmt.Errorf("secret %s is empty", id)
	case size > resource.MaxSecretBytes:
		return nil, fmt.Errorf("secret %s is larger than %d bytes", id, resource.MaxSecretBytes)
	}
	inNamespace, inCluster := size, size
	for otherID, other := range s.Secrets {
		if otherID == id {
			continue
		}
		inCluster += ClearSize(other.Spec.Value)
		if other.Namespace == payload.Namespace {
			inNamespace += ClearSize(other.Spec.Value)
		}
	}
	switch {
	case inNamespace > resource.MaxNamespaceSecretBytes:
		return nil, fmt.Errorf("secret %s: the secrets of namespace %s would be larger than %d bytes",
			id, payload.Namespace, resource.MaxNamespaceSecretBytes)
	case inCluster > resource.MaxClusterSecretBytes:
		return nil, fmt.Errorf("secret %s: the secrets of the cluster would be larger than %d bytes",
			id, resource.MaxClusterSecretBytes)
	}
	if !exists {
		stored = &Secret{Meta: Meta{Namespace: payload.Namespace, Name: payload.Name, Owner: payload.Owner}}
		s.Secrets[id] = stored
	}
	stored.Spec = spec
	return &stored.Meta, nil
}

func removeSecret(s *State, id string) error {
	delete(s.Secrets, id)
	return nil
}

// The length of what age adds when it seals for the cluster's one X25519
// recipient (datakey.Key.Seal).
const (
	// sealedHeader is the header ("age-encryption.org/v1", one recipient
	// stanza, the MAC) and the payload's nonce.
	sealedHeader = 168 + 16
	// sealedChunk is how much clear text age seals at a time; each chunk
	// gets a tag of sealedTag bytes.
	sealedChunk = 64 << 10
	sealedTag   = 16
)

// ClearSize is how long a sealed value is in clear, worked out from its
// length, so the state can hold the limits without opening the value; -1
// for what is too short to be sealed. A test in datakey holds it against
// real sealing, so a change in age's format fails there.
func ClearSize(sealed Sealed) int {
	body := len(sealed) - sealedHeader
	if body < sealedTag {
		return -1
	}
	chunks := (body + sealedChunk + sealedTag - 1) / (sealedChunk + sealedTag)
	return body - chunks*sealedTag
}
