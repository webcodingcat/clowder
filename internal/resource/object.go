package resource

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/webcodingcat/clowder/internal/clusterspec"
	"github.com/webcodingcat/clowder/internal/units"
)

// APIVersion is the version of the resources' format. clowder refuses a
// resource of another version.
const APIVersion = "clowder/v1"

// Kinds are the kinds of resources.
var Kinds = []string{KindClusterConfiguration, KindNamespace, KindSecret, KindStack, KindVolume}

// KindOf returns the kind that word names. The word is the kind in lower
// case ("volume"), as in the API's paths.
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
	// Spec is the kind's spec, such as a VolumeSpec, SecretSpec,
	// SealedSecretSpec or stackspec.Spec. A namespace may have none.
	Spec json.RawMessage `json:"spec,omitempty"`
	// Status is what the cluster reports about the resource. An apply
	// ignores it.
	Status json.RawMessage `json:"status,omitempty"`
}

// Metadata names a resource. The cluster sets Owner, Generation and
// ResourceVersion, and an apply ignores them. So a resource that the API
// returned can be applied again as it is. A secret is the exception,
// because its values never leave the cluster.
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

// String names the resource as messages show it: "Volume shop/data".
func (r Ref) String() string {
	if r.Namespace == "" {
		return r.Kind + " " + r.Name
	}
	return r.Kind + " " + r.Namespace + "/" + r.Name
}

// decodeStrict reads a spec as users write it. A field that the kind does
// not have is an error, so a typo is not silently ignored.
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

// DecodeNamespaceSpec reads and checks a namespace's spec as users write
// it. A namespace may have no spec.
func DecodeNamespaceSpec(raw json.RawMessage) (NamespaceSpec, error) {
	var spec NamespaceSpec
	if err := decodeStrict(raw, &spec); err != nil {
		return NamespaceSpec{}, err
	}
	return spec, spec.Check()
}

// DecodeClusterConfiguration reads the configuration's spec as users write
// it. It does not check the spec. The state runs clusterspec.Check, and it
// also checks the spec against the cluster.
func DecodeClusterConfiguration(raw json.RawMessage) (clusterspec.Config, error) {
	var spec clusterspec.Config
	if err := decodeStrict(raw, &spec); err != nil {
		return clusterspec.Config{}, err
	}
	return spec, nil
}

// DecodeVolumeSpec reads and checks a volume's spec as users write it.
func DecodeVolumeSpec(raw json.RawMessage) (VolumeSpec, error) {
	// replicas has no default: 0 and 1 are both common but mean very
	// different things, so a missing value must differ from 0
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

// DecodeSecretSpec reads a Secret's spec as users write it and returns its
// values by key.
func DecodeSecretSpec(raw json.RawMessage) (map[string][]byte, error) {
	var spec SecretSpec
	if err := decodeStrict(raw, &spec); err != nil {
		return nil, err
	}
	return spec.Values()
}

// DecodeSealedSecretSpec reads and checks a SealedSecret's spec as users
// write it.
func DecodeSealedSecretSpec(raw json.RawMessage) (SealedSecretSpec, error) {
	var spec SealedSecretSpec
	if err := decodeStrict(raw, &spec); err != nil {
		return SealedSecretSpec{}, err
	}
	return spec, spec.Check()
}
