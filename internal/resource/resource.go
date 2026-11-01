// Package resource is what users apply to the cluster besides cluster.yaml:
// the kinds of resources, their specs and what a spec must be on its own.
// A stack's spec has a package of its own (stackspec). The package does no
// I/O: the CLI, the API and the state share it.
package resource

import (
	"errors"
	"fmt"
	"strings"

	"github.com/webcodingcat/clowder/internal/secretvalue"
	"github.com/webcodingcat/clowder/internal/units"
)

// The kinds of resources. A namespace holds the others.
const (
	KindNamespace = "Namespace"
	KindVolume    = "Volume"
	KindSecret    = "Secret"
	// KindStack's spec is a stackspec.Spec.
	KindStack = "Stack"
)

// Namespaced tells whether the resources of a kind live in a namespace.
func Namespaced(kind string) bool {
	return kind != KindNamespace
}

// VolumeSpec is a volume as the user asks for it. Nothing changes an
// existing volume yet: its class, size and replicas stay as created.
type VolumeSpec struct {
	// Class is a storage class of cluster.yaml.
	Class string     `yaml:"class" json:"class"`
	Size  units.Size `yaml:"size" json:"size"`
	// Replicas are the copies DRBD keeps in step, 1 to 3; 0 is one plain LV
	// without DRBD, whose data a move of its stack does not keep.
	Replicas int `yaml:"replicas" json:"replicas"`
}

// Check tells what is wrong with the spec on its own; whether the class
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
// existing volume: nothing changes one yet, so such an apply would seem to
// work and do nothing.
func (v VolumeSpec) CheckUnchanged(wanted VolumeSpec) error {
	if wanted == v {
		return nil
	}
	return fmt.Errorf("exists with class %s, size %s and %d replicas; clowder cannot change a volume yet",
		v.Class, units.FormatSize(int64(v.Size)), v.Replicas)
}

// The limits of the secrets, their values in clear: every apply writes them
// into the Raft log, and every snapshot holds them. The cluster's count
// both the resources and cluster.yaml's values.
const (
	MaxSecretBytes          = 64 << 10
	MaxNamespaceSecretBytes = 256 << 10
	MaxClusterSecretBytes   = 4 << 20
	MaxClusterSecrets       = 1000
)

// SecretSpec is a secret as it travels to the cluster: one value, still
// encrypted (secretvalue's armor, for the secret's ID), as the user's file
// has it tagged !secret in spec.value. In the file the field is value, as
// that is what stands there; on the wire it is encrypted, as that is what
// it carries. The cluster opens it, checks what it is for and seals it;
// the size of the value in clear is the state's to hold.
type SecretSpec struct {
	Encrypted string `json:"encrypted"`
}

// Check tells what is wrong with the spec on its own.
func (s SecretSpec) Check() error {
	if !strings.HasPrefix(s.Encrypted, secretvalue.ArmorHeader) {
		return errors.New("the value is not encrypted (clowder secret encrypt)")
	}
	return nil
}
