// Package resource is what users apply to the cluster: the kinds of
// resources, their specs, and the rules that a spec must meet on its own.
// A stack's spec has a package of its own (stackspec), and so does the
// cluster's configuration (clusterspec). The package does no I/O, because
// the CLI, the API and the state share it.
package resource

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/secretvalue"
	"github.com/webcodingcat/clowder/internal/units"
)

// The kinds of resources. A namespace holds the other kinds, except the
// configuration.
const (
	KindNamespace = "Namespace"
	KindVolume    = "Volume"
	KindSecret    = "Secret"
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

// SecretSpec is a secret as it travels to the cluster: its values by key,
// each still encrypted (secretvalue's armor, for the secret's ID), as the
// user's file has them tagged !secret in spec.data. In the file the field
// is called data, because that is what it holds there; on the wire it is
// called encrypted, because that is what it carries. The cluster decrypts
// each value, checks what it is for and seals it. The state enforces the
// limits on the size of the values in clear.
type SecretSpec struct {
	Encrypted map[string]string `json:"encrypted"`
}

// Check tells what is wrong with the spec on its own.
func (s SecretSpec) Check() error {
	if len(s.Encrypted) == 0 {
		return errors.New("a secret needs at least one key in data")
	}
	var problems []error
	for _, key := range slices.Sorted(maps.Keys(s.Encrypted)) {
		switch {
		case !names.ValidSecretKey(key):
			problems = append(problems, fmt.Errorf("key %q: %s", key, names.SecretKeyRule))
		case !strings.HasPrefix(s.Encrypted[key], secretvalue.ArmorHeader):
			problems = append(problems, fmt.Errorf("key %s: the value is not encrypted (clowder secret encrypt)", key))
		}
	}
	return errors.Join(problems...)
}
