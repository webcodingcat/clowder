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

// The resources as users read them: lists, one resource, and the statuses
// they show. What changes them is in changes.go.

// ResourceList is the answer to GET /v1/resources/{kind}: the resources
// sorted by namespace and name, and the state's index they were read at.
type ResourceList struct {
	Index uint64            `json:"index"`
	Items []resource.Object `json:"items"`
}

// VolumeStatus is a volume's status as the API shows it. Status is the
// state's (pending, ok) for a volume that has all its copies synced,
// else resyncing while a copy catches up, or degraded while copies are
// missing.
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
	// UpError is why its host could not bring a failing stack up.
	UpError string `json:"upError,omitempty"`
	// AppliedSecrets are the digests of the secrets its containers came up
	// with: one that differs from the secret's own has not reached them yet.
	AppliedSecrets map[string]string `json:"appliedSecrets,omitempty"`
	// Restarted is when each service was last restarted (stack restart).
	Restarted map[string]time.Time `json:"restarted,omitempty"`
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
// namespace, or none for the kinds that live in none.
type namespaceSelection struct {
	namespace string
	all       bool
}

func (n namespaceSelection) holds(namespace string) bool {
	return n.all || namespace == n.namespace
}

// selectNamespace reads ?namespace= or ?allNamespaces=true for a kind and
// asks Authorize whether the caller may read it. There is no default, so no
// reader gets the wrong namespace by accident. ok is false (and the answer
// written) when the request does not fit.
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
	if err := state.Authorize(actorOf(identityOf(r)), state.VerbRead, kind, selected.namespace); err != nil {
		writeError(w, http.StatusForbidden, err)
		return selected, false
	}
	return selected, true
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
		Status: string(stack.Status), PushedOut: stack.PushedOut, WaitReason: stack.WaitReason, UpError: stack.UpError,
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
	for _, volumeCopy := range volume.Copies {
		host := strconv.Itoa(int(volumeCopy.Host))
		if known, ok := current.Hosts[volumeCopy.Host]; ok {
			host = known.Name
		}
		status.Copies = append(status.Copies, VolumeCopy{
			Host: host, Class: volumeCopy.Class, Tiebreaker: volumeCopy.Tiebreaker, Synced: volumeCopy.Synced,
		})
	}
	if volume.Status != state.VolumePending {
		status.Status = volumeHealth(volume)
	}
	return status
}

// The health of a placed volume beside ok.
const (
	volumeDegraded  = "degraded"
	volumeResyncing = "resyncing"
)

// volumeHealth tells how a placed volume's copies are: degraded with fewer
// copies than replicas, resyncing while one is not synced yet, else ok.
// Replicas 0 has one plain LV.
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
