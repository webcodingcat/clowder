package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/webcodingcat/clowder/internal/resource"
	"github.com/webcodingcat/clowder/internal/stackspec"
	"github.com/webcodingcat/clowder/internal/state"
)

// ResourceList is the answer to GET /v1/resources/{kind}, sorted by
// namespace and name. Index is the state's index at which it was read, for
// the watch.
type ResourceList struct {
	Index uint64            `json:"index"`
	Items []resource.Object `json:"items"`
}

// VolumeStatus is a volume's status as the API shows it. Status adds
// resyncing and degraded to the state's status (volumeHealth).
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
	Synced     bool   `json:"synced,omitempty"`
}

// SecretStatus is a secret's status: its keys and its digest, never its
// values.
type SecretStatus struct {
	Keys   []string `json:"keys"`
	Digest string   `json:"digest"`
}

// StackStatus is a stack's status as the API shows it.
type StackStatus struct {
	Status string `json:"status"`
	Host   string `json:"host,omitempty"` // empty: not placed yet
	// Priority is the stack's priority as the cluster resolves it now.
	// Default says that it is the configuration's default.
	Priority  int      `json:"priority"`
	Default   bool     `json:"default,omitempty"`
	PushedOut []string `json:"pushedOut,omitempty"`
	// WaitReason is why the stack, or a volume it names, finds no place.
	WaitReason string `json:"waitReason,omitempty"`
	// UpError is why the host could not bring up a failing stack.
	UpError string `json:"upError,omitempty"`
	// AppliedSecrets are the digests of the secrets that the containers
	// started with. A digest that differs from the secret's own means that
	// the change has not reached the containers yet.
	AppliedSecrets map[string]string `json:"appliedSecrets,omitempty"`
	// Restarted is when each service was last restarted (stack restart).
	Restarted map[string]time.Time `json:"restarted,omitempty"`
}

func (s *Server) listResources(w http.ResponseWriter, r *http.Request) {
	kind, ok := resource.KindOf(r.PathValue("kind"))
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Errorf("no kind of resource %q", r.PathValue("kind")))
		return
	}
	selected, ok := selectNamespace(w, r, kind)
	if !ok {
		return
	}
	list := ResourceList{Items: []resource.Object{}}
	s.config.Cluster.Read(func(current *state.State) {
		list.Index = current.Index
		for _, object := range objects(current, kind) {
			if selected.holds(object.Metadata.Namespace) {
				list.Items = append(list.Items, object)
			}
		}
	})
	writeJSON(w, http.StatusOK, list)
}

// namespaceSelection is what a list or a watch reads: one namespace, every
// namespace, or none for the kinds that have no namespace.
type namespaceSelection struct {
	namespace string
	all       bool
}

func (n namespaceSelection) holds(namespace string) bool {
	return n.all || namespace == n.namespace
}

// selectNamespace has no default, so no reader gets the wrong namespace by
// accident. If the query does not fit the kind, it writes the error answer
// and returns false.
func selectNamespace(w http.ResponseWriter, r *http.Request, kind string) (namespaceSelection, bool) {
	selected := namespaceSelection{
		namespace: r.URL.Query().Get("namespace"), all: r.URL.Query().Get("allNamespaces") == "true",
	}
	exactlyOne := (selected.namespace != "") != selected.all
	switch {
	case !resource.Namespaced(kind) && (selected.namespace != "" || selected.all):
		writeError(w, http.StatusBadRequest, fmt.Errorf("a %s lives in no namespace", kind))
		return selected, false
	case resource.Namespaced(kind) && !exactlyOne:
		writeError(w, http.StatusBadRequest, errors.New("name either namespace= or allNamespaces=true"))
		return selected, false
	}
	return selected, true
}

func (s *Server) getResource(w http.ResponseWriter, r *http.Request) {
	kind, ok := resource.KindOf(r.PathValue("kind"))
	if !ok || !resource.Namespaced(kind) {
		writeError(w, http.StatusNotFound, fmt.Errorf("no resource at %s", r.URL.Path))
		return
	}
	s.answerResource(w, resource.Ref{Kind: kind, Namespace: r.PathValue("namespace"), Name: r.PathValue("name")})
}

func (s *Server) getClusterResource(w http.ResponseWriter, r *http.Request) {
	kind, ok := resource.KindOf(r.PathValue("kind"))
	if !ok || resource.Namespaced(kind) {
		writeError(w, http.StatusNotFound, fmt.Errorf("no resource at %s", r.URL.Path))
		return
	}
	s.answerResource(w, resource.Ref{Kind: kind, Name: r.PathValue("name")})
}

func (s *Server) answerResource(w http.ResponseWriter, ref resource.Ref) {
	var found resource.Object
	var ok bool
	s.config.Cluster.Read(func(current *state.State) { found, ok = objectOf(current, ref) })
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Errorf("no %s", ref))
		return
	}
	writeJSON(w, http.StatusOK, found)
}

func objectOf(current *state.State, ref resource.Ref) (resource.Object, bool) {
	for _, object := range objects(current, ref.Kind) {
		if object.Ref() == ref {
			return object, true
		}
	}
	return resource.Object{}, false
}

// objects sorts by namespace and name. A secret shows its digest, never
// its values.
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
		// the API's own types always encode without error
		if spec != nil {
			object.Spec, _ = json.Marshal(spec)
		}
		if status != nil {
			object.Status, _ = json.Marshal(status)
		}
		shown = append(shown, object)
	}
	switch kind {
	case resource.KindClusterConfiguration:
		if current.Configured() {
			add(current.Config.Meta, current.Config.Spec, nil)
		}
	case resource.KindNamespace:
		for _, name := range slices.Sorted(maps.Keys(current.Namespaces)) {
			namespace := current.Namespaces[name]
			var spec any
			if len(namespace.Spec.ImagePullSecrets) > 0 {
				spec = namespace.Spec
			}
			add(namespace.Meta, spec, nil)
		}
	case resource.KindVolume:
		for _, id := range slices.Sorted(maps.Keys(current.Volumes)) {
			volume := current.Volumes[id]
			add(volume.Meta, volume.Spec, volumeStatus(current, volume))
		}
	case resource.KindSecret:
		for _, id := range slices.Sorted(maps.Keys(current.Secrets)) {
			secret := current.Secrets[id]
			add(secret.Meta, nil, SecretStatus{Keys: secret.Spec.Keys(), Digest: secret.Spec.Digest})
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
		Status: string(stack.Status), PushedOut: stack.PushedOut, WaitReason: stack.WaitReason, UpError: stack.UpError,
		AppliedSecrets: stack.AppliedSecrets, Restarted: stack.Restarted, Default: stack.Spec.Priority == "",
	}
	if stack.Host != nil {
		if host, ok := current.Hosts[*stack.Host]; ok {
			status.Host = host.Name
		}
	}
	// the state checked that the priority resolves, at the apply and at
	// every configuration change since
	written, _ := stackspec.ParsePriority(stack.Spec.Priority)
	status.Priority, _ = current.Config.Spec.WithDefaults().Priorities.Resolve(written)
	return status
}

func volumeStatus(current *state.State, volume *state.Volume) VolumeStatus {
	status := VolumeStatus{Status: string(volume.Status), DataGeneration: volume.DataGeneration}
	for _, volumeCopy := range volume.Copies {
		host := strconv.Itoa(int(volumeCopy.Host))
		if known, ok := current.Hosts[volumeCopy.Host]; ok {
			host = known.Name
		}
		status.Copies = append(status.Copies, VolumeCopy{
			Host: host, Class: volumeCopy.Class, Tiebreaker: volumeCopy.Tiebreaker, Synced: volumeCopy.Synced,
		})
	}
	// pending and restore-pending need no more detail
	if volume.Status == state.VolumeOK {
		status.Status = volumeHealth(volume)
	}
	return status
}

// The health values of a placed volume other than ok.
const (
	volumeDegraded  = "degraded"
	volumeResyncing = "resyncing"
)

// volumeHealth counts a volume with replicas 0 as one copy, because it has
// one plain LV.
func volumeHealth(volume *state.Volume) string {
	copies, synced := 0, 0
	for _, volumeCopy := range volume.Copies {
		if volumeCopy.Tiebreaker {
			continue
		}
		copies++
		if volumeCopy.Synced {
			synced++
		}
	}
	switch {
	case copies < max(volume.Spec.Replicas, 1):
		return volumeDegraded
	case synced < copies:
		return volumeResyncing
	}
	return string(state.VolumeOK)
}
