// Package resource is what users apply to the cluster: the kinds of
// resources, their specs, and the rules that a spec must meet on its own.
// A stack's spec has a package of its own (stackspec), and so does the
// cluster's configuration (clusterspec). The package does no I/O, because
// the CLI, the API and the state share it.
package resource

import (
	"encoding/base64"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/units"
)

// The kinds of resources. A namespace holds the other kinds, except the
// configuration.
const (
	KindNamespace = "Namespace"
	KindVolume    = "Volume"
	KindSecret    = "Secret"
	// KindSealedSecret is the way of a Secret into Git: its values are
	// sealed for the cluster's sealing key, as Bitnami's sealed secrets
	// are. It is only a way in: the cluster stores it as the Secret of the
	// same namespace and name (StoredKind), and get, delete and diff work
	// on that Secret.
	KindSealedSecret = "SealedSecret"
	// KindStack's spec is a stackspec.Spec.
	KindStack = "Stack"
	// KindClusterConfiguration's spec is a clusterspec.Config. There is
	// one per cluster, named ClusterConfigurationName, like kubeadm's
	// ClusterConfiguration.
	KindClusterConfiguration = "ClusterConfiguration"
)

// ClusterConfigurationName is the name of the one ClusterConfiguration, as
// OpenShift names its configuration resources "cluster".
const ClusterConfigurationName = "cluster"

// StoredKind returns the kind that the cluster stores a resource of kind
// as: a SealedSecret becomes a Secret, every other kind stays itself.
func StoredKind(kind string) string {
	if kind == KindSealedSecret {
		return KindSecret
	}
	return kind
}

// Namespaced tells whether the resources of a kind live in a namespace.
func Namespaced(kind string) bool {
	return kind != KindNamespace && kind != KindClusterConfiguration
}

// NamespaceSpec is a namespace as the user asks for it.
type NamespaceSpec struct {
	// ImagePullSecrets name Secrets of the namespace that hold registry
	// logins (the keys of RegistryLoginKeys). The stacks' images are pulled
	// with them, as Kubernetes pulls with those of a namespace's default
	// service account. They are not checked at apply, because the Secrets
	// live in the namespace and so come after it.
	ImagePullSecrets []LocalObjectReference `json:"imagePullSecrets,omitempty"`
}

// LocalObjectReference names a resource in the same namespace as the
// resource that names it, as in Kubernetes.
type LocalObjectReference struct {
	Name string `json:"name"`
}

// The keys of a Secret that holds a registry login.
const (
	RegistryServerKey   = "server"
	RegistryUsernameKey = "username"
	RegistryPasswordKey = "password"
)

// RegistryLoginKeys are the keys that a Secret of imagePullSecrets has.
var RegistryLoginKeys = []string{RegistryServerKey, RegistryUsernameKey, RegistryPasswordKey}

// Check tells what is wrong with the spec on its own.
func (n NamespaceSpec) Check() error {
	var problems []error
	seen := map[string]bool{}
	for _, secret := range n.ImagePullSecrets {
		switch {
		case !names.Valid(secret.Name):
			problems = append(problems, fmt.Errorf("imagePullSecrets: %q is not a secret name (%s)", secret.Name, names.Rule))
		case seen[secret.Name]:
			problems = append(problems, fmt.Errorf("imagePullSecrets: %s is named twice", secret.Name))
		}
		seen[secret.Name] = true
	}
	return errors.Join(problems...)
}

// VolumeSpec is a volume as the user asks for it. Nothing changes an
// existing volume yet: its class, size and replicas stay as created.
type VolumeSpec struct {
	// Class is a storage class of the configuration.
	Class string     `json:"class"`
	Size  units.Size `json:"size"`
	// Replicas are the copies that DRBD keeps in sync, 1 to 3. 0 is one
	// plain LV without DRBD, whose data is not kept when its stack moves.
	Replicas int `json:"replicas"`
}

// Check tells what is wrong with the spec on its own. Whether the class
// exists depends on the cluster.
func (v VolumeSpec) Check() error {
	var problems []error
	if v.Class == "" {
		problems = append(problems, errors.New("class is required"))
	}
	if v.Size <= 0 {
		problems = append(problems, errors.New("size is required"))
	}
	if v.Replicas < 0 || v.Replicas > 3 {
		problems = append(problems, fmt.Errorf("replicas %d is not from 0 to 3", v.Replicas))
	}
	return errors.Join(problems...)
}

// CheckUnchanged refuses another class, size or number of replicas for an
// existing volume. Nothing can change an existing volume yet, so such an
// apply would seem to work but do nothing.
func (v VolumeSpec) CheckUnchanged(wanted VolumeSpec) error {
	if wanted == v {
		return nil
	}
	return fmt.Errorf("exists with class %s, size %s and %d replicas; clowder cannot change a volume yet",
		v.Class, units.FormatSize(int64(v.Size)), v.Replicas)
}

// The limits of the secrets, counted by their values in clear. Every apply
// writes them into the Raft log, and every snapshot holds them.
// MaxSecretBytes limits all values of one Secret together.
const (
	MaxSecretBytes          = 64 << 10
	MaxNamespaceSecretBytes = 256 << 10
	MaxClusterSecretBytes   = 4 << 20
	MaxClusterSecrets       = 1000
)

// SecretSpec is a Secret as users write it, with its values in clear, as
// in Kubernetes: data holds each value in Base64, stringData as text, and
// for a key in both, stringData wins. Unlike in Kubernetes, both stand in
// spec, as every clowder resource keeps what it wants there. The values
// travel to the cluster over mTLS, and the cluster encrypts them with its
// data key. The state enforces the limits on the size of the values.
type SecretSpec struct {
	Data       map[string]string `json:"data,omitempty"`
	StringData map[string]string `json:"stringData,omitempty"`
}

// Values returns the Secret's values by key, or what is wrong with the
// spec. No error names a value.
func (s SecretSpec) Values() (map[string][]byte, error) {
	values := map[string][]byte{}
	var problems []error
	for _, key := range slices.Sorted(maps.Keys(s.Data)) {
		if _, wins := s.StringData[key]; wins {
			continue
		}
		// base64's errors name only an offset, never the text
		value, err := base64.StdEncoding.DecodeString(s.Data[key])
		if err != nil {
			problems = append(problems, fmt.Errorf("data: key %s: the value is not Base64 (or use stringData): %w", key, err))
			continue
		}
		values[key] = value
	}
	for _, key := range slices.Sorted(maps.Keys(s.StringData)) {
		values[key] = []byte(s.StringData[key])
	}
	if len(values) == 0 && len(problems) == 0 {
		return nil, errors.New("a secret needs at least one key in data or stringData")
	}
	problems = append(problems, checkSecretKeys(slices.Collect(maps.Keys(values))))
	if err := errors.Join(problems...); err != nil {
		return nil, err
	}
	return values, nil
}

// SealedSecretSpec is a SealedSecret as users write it: each value sealed
// for the cluster's sealing key and for the Secret's ID (secretvalue), as
// clowder secret seal writes it. Only the cluster opens the values.
type SealedSecretSpec struct {
	EncryptedData map[string]string `json:"encryptedData"`
}

// Check tells what is wrong with the spec on its own.
func (s SealedSecretSpec) Check() error {
	if len(s.EncryptedData) == 0 {
		return errors.New("a sealed secret needs at least one key in encryptedData")
	}
	return checkSecretKeys(slices.Collect(maps.Keys(s.EncryptedData)))
}

// checkSecretKeys tells which keys of a Secret break names.SecretKeyRule.
func checkSecretKeys(keys []string) error {
	var problems []error
	for _, key := range slices.Sorted(slices.Values(keys)) {
		if !names.ValidSecretKey(key) {
			problems = append(problems, fmt.Errorf("key %q: %s", key, names.SecretKeyRule))
		}
	}
	return errors.Join(problems...)
}
