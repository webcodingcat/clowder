package resource

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/webcodingcat/clowder/internal/units"
)

// APIVersion is the version of the resources' format; a resource of
// another is refused.
const APIVersion = "clowder/v1"

// Kinds are the kinds of resources.
var Kinds = []string{KindNamespace, KindSecret, KindVolume}

// KindOf is the kind a word names: the kind in lower case ("volume"), as
// the API's paths write it.
func KindOf(word string) (string, bool) {
	for _, kind := range Kinds {
		if strings.ToLower(kind) == word {
			return kind, true
		}
	}
	return "", false
}

// Object is a resource as users write it and as the API answers it.
type Object struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Metadata   Metadata `json:"metadata"`
	// Spec is the kind's (VolumeSpec, SecretSpec; a namespace has none).
	Spec json.RawMessage `json:"spec,omitempty"`
	// Status is what the cluster tells about the resource; an apply
	// ignores it.
	Status json.RawMessage `json:"status,omitempty"`
}

// Metadata names a resource. The cluster sets Owner, Generation and
// ResourceVersion; an apply ignores them, so what the API answered applies
// again as it is, but for a secret, whose value never leaves the cluster.
type Metadata struct {
	Namespace       string `json:"namespace,omitempty"`
	Name            string `json:"name"`
	Owner           string `json:"owner,omitempty"`
	Generation      int64  `json:"generation,omitempty"`
	ResourceVersion uint64 `json:"resourceVersion,omitempty"`
}

// Ref names a resource without its spec, as a delete does.
type Ref struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
}

// Ref names the object.
func (o Object) Ref() Ref {
	return Ref{Kind: o.Kind, Namespace: o.Metadata.Namespace, Name: o.Metadata.Name}
}

// String is how messages name a resource: "Volume shop/data".
func (r Ref) String() string {
	if r.Namespace == "" {
		return r.Kind + " " + r.Name
	}
	return r.Kind + " " + r.Namespace + "/" + r.Name
}

// decodeStrict reads a spec as users write it: a field the kind does not
// have is an error, not ignored.
func decodeStrict(raw json.RawMessage, spec any) error {
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(spec); err != nil {
		return fmt.Errorf("read the spec: %w", err)
	}
	return nil
}

// CheckNamespaceSpec refuses a spec for a namespace, which has none.
func CheckNamespaceSpec(raw json.RawMessage) error {
	var spec map[string]json.RawMessage
	if err := decodeStrict(raw, &spec); err != nil {
		return err
	}
	if len(spec) > 0 {
		return errors.New("a namespace has no spec")
	}
	return nil
}

// DecodeVolumeSpec reads and checks a volume's spec as users write it.
func DecodeVolumeSpec(raw json.RawMessage) (VolumeSpec, error) {
	// replicas has no default: 0 and 1 are both common and mean very
	// different things, so a missing one must be told from 0
	var written struct {
		Class    string     `json:"class"`
		Size     units.Size `json:"size"`
		Replicas *int       `json:"replicas"`
	}
	if err := decodeStrict(raw, &written); err != nil {
		return VolumeSpec{}, err
	}
	if written.Replicas == nil {
		return VolumeSpec{}, errors.New("replicas is required: 1 to 3 copies with DRBD, or 0 for a plain LV")
	}
	spec := VolumeSpec{Class: written.Class, Size: written.Size, Replicas: *written.Replicas}
	return spec, spec.Check()
}

// DecodeSecretSpec reads and checks a secret's spec as users write it.
func DecodeSecretSpec(raw json.RawMessage) (SecretSpec, error) {
	var spec SecretSpec
	if err := decodeStrict(raw, &spec); err != nil {
		return SecretSpec{}, err
	}
	return spec, spec.Check()
}
