package state

import (
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/webcodingcat/clowder/internal/clusterspec"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/resource"
)

// Configured tells whether a ClusterConfiguration was applied. Before that,
// there is no Hetzner token, no Floating IP, no ACME and no alerts.
func (s *State) Configured() bool {
	return s.Config.Generation > 0
}

// SystemSecret returns the encrypted value that a reference names in
// names.System, or false if there is none.
func (s *State) SystemSecret(ref names.SecretKeyRef) (Encrypted, bool) {
	secret, ok := s.Secrets[names.ID(names.System, ref.Name)]
	if !ok {
		return nil, false
	}
	value, ok := secret.Spec.Data[ref.Key]
	return value, ok
}

// ErrNotConfigured is returned for the Hetzner token of a cluster without
// a configuration.
var ErrNotConfigured = errors.New("the cluster has no ClusterConfiguration yet")

// HetznerToken returns the encrypted token that the configuration names.
func (s *State) HetznerToken() (Encrypted, error) {
	if !s.Configured() {
		return nil, ErrNotConfigured
	}
	ref := s.Config.Spec.Hetzner.TokenSecretRef
	token, ok := s.SystemSecret(ref)
	if !ok {
		// the state refuses a configuration that names a value it lacks
		return nil, fmt.Errorf("the Hetzner token %s is not among the Secrets of %s", ref, names.System)
	}
	return token, nil
}

// RegistryLogin is the login for one private image registry. Each part is
// encrypted, as its Secret stores it.
type RegistryLogin struct {
	Secret                     string
	Server, Username, Password Encrypted
}

// RegistryLogins returns the logins that a namespace's stacks pull their
// images with, in the order of its imagePullSecrets. Like the kubelet, it
// skips a Secret that is missing or lacks a key, and goes on. So an image
// that needs no login still pulls. missing says which Secrets it skipped
// and why. The stack's host names them when a pull fails.
func (s *State) RegistryLogins(namespace string) (logins []RegistryLogin, missing error) {
	held, ok := s.Namespaces[namespace]
	if !ok {
		return nil, fmt.Errorf("no namespace %s", namespace)
	}
	var problems []error
	for _, ref := range held.Spec.ImagePullSecrets {
		secret, ok := s.Secrets[names.ID(namespace, ref.Name)]
		if !ok {
			problems = append(problems, fmt.Errorf("namespace %s names the image pull secret %s, which it does not have",
				namespace, ref.Name))
			continue
		}
		var parts [3]Encrypted
		lacks := slices.IndexFunc(resource.RegistryLoginKeys, func(key string) bool {
			_, has := secret.Spec.Data[key]
			return !has
		})
		if lacks >= 0 {
			problems = append(problems, fmt.Errorf("the image pull secret %s of namespace %s has no key %s",
				ref.Name, namespace, resource.RegistryLoginKeys[lacks]))
			continue
		}
		for index, key := range resource.RegistryLoginKeys {
			parts[index] = secret.Spec.Data[key]
		}
		logins = append(logins, RegistryLogin{Secret: ref.Name, Server: parts[0], Username: parts[1], Password: parts[2]})
	}
	return logins, errors.Join(problems...)
}

func findConfig(s *State, id string) *Meta {
	if id == resource.ClusterConfigurationName && s.Configured() {
		return &s.Config.Meta
	}
	return nil
}

// putConfig replaces the configuration. It refuses a configuration that an
// existing resource would no longer pass:
//   - one without a storage class that a volume or one of its copies has;
//   - one without a priority category that a stack names;
//   - one without the entry in stacks.privileged that a stack needs;
//   - one that puts a host's underlay outside the private network.
//
// It also refuses a configuration that names a value that names.System
// lacks. The state checks all this itself, in the log's order, so no
// resource can slip in between.
func putConfig(s *State, id string, payload PutResource) (func() *Meta, error) {
	if id != resource.ClusterConfigurationName {
		return nil, fmt.Errorf("the ClusterConfiguration is named %s, not %s", resource.ClusterConfigurationName, id)
	}
	written, err := decodeSpec[clusterspec.Config](payload.Kind, id, payload.Spec)
	if err != nil {
		return nil, err
	}
	spec := written.WithDefaults()
	if s.Configured() && sameJSON(s.Config.Spec, spec) {
		return nil, errUnchanged
	}
	if problems := clusterspec.Check(written); len(problems) > 0 {
		return nil, problems
	}
	for _, place := range clusterspec.SecretPlaces(spec) {
		if _, ok := s.SystemSecret(place.Ref); !ok {
			return nil, fmt.Errorf("%s: %s has no Secret %s with the key %s; apply it above the configuration",
				place.Path, names.System, place.Ref.Name, place.Ref.Key)
		}
	}
	for _, hostID := range slices.Sorted(maps.Keys(s.Hosts)) {
		if host := s.Hosts[hostID]; host.Phase != Removed {
			if err := checkUnderlay(spec, host); err != nil {
				return nil, err
			}
		}
	}
	for _, stackID := range slices.Sorted(maps.Keys(s.Stacks)) {
		stack := s.Stacks[stackID]
		if stack.Spec.Privileged() && !slices.Contains(spec.Stacks.Privileged, stackID) {
			return nil, fmt.Errorf("stack %s asks for privileges, so it must stay in stacks.privileged", stackID)
		}
		if _, err := servicesOf(stack.Spec, spec.Priorities); err != nil {
			return nil, fmt.Errorf("the configuration would break stack %s: %w", stackID, err)
		}
	}
	// a volume's class, and the class of each copy's pool (a fallback of it)
	for _, volumeID := range slices.Sorted(maps.Keys(s.Volumes)) {
		volume := s.Volumes[volumeID]
		classes := []string{volume.Spec.Class}
		for _, volumeCopy := range volume.Copies {
			classes = append(classes, volumeCopy.Class)
		}
		for _, class := range classes {
			if _, ok := spec.StorageClasses[class]; !ok {
				return nil, fmt.Errorf("volume %s has storage class %s, so the class must stay", volumeID, class)
			}
		}
	}
	return func() *Meta {
		if !s.Configured() {
			s.Config.Meta = Meta{Name: id, Owner: payload.Owner}
		}
		s.Config.Spec = spec
		return &s.Config.Meta
	}, nil
}

// removeConfig always refuses: without a configuration, fencing and the
// Floating IP would stop silently.
func removeConfig(*State, string) (func(), error) {
	return nil, errors.New("the ClusterConfiguration cannot be deleted, only changed: fencing and the Floating IP need it")
}

// configNaming returns the first field of the configuration that names a
// key that the Secret name in names.System would no longer have. keep holds
// the keys that the Secret keeps, and nil keeps none. It returns false if
// there is no such field.
func (s *State) configNaming(name string, keep map[string]Encrypted) (clusterspec.SecretPlace, bool) {
	if !s.Configured() {
		return clusterspec.SecretPlace{}, false
	}
	for _, place := range clusterspec.SecretPlaces(s.Config.Spec) {
		if _, kept := keep[place.Ref.Key]; place.Ref.Name == name && !kept {
			return place, true
		}
	}
	return clusterspec.SecretPlace{}, false
}
