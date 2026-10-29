package state

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/webcodingcat/clowder/internal/clusterdir"
	"github.com/webcodingcat/clowder/internal/names"
)

// ApplyConfig replaces cluster.yaml and the secrets with what the user
// applied. JoinKeys is the whole list of valid bootstrap keys: a key left
// out is invalid from now on.
type ApplyConfig struct {
	Hash       string            `json:"hash"`
	Spec       clusterdir.Config `json:"spec"`
	Secrets    map[string]Sealed `json:"secrets"`
	JoinKeys   []Sealed          `json:"join_keys"`
	Registries map[string]Sealed `json:"registries"`
}

func applyConfig(s *State, _ Command, payload ApplyConfig) error {
	if problems := clusterdir.Check(payload.Spec); len(problems) > 0 {
		return problems
	}
	for _, name := range clusterdir.SecretNames(payload.Spec) {
		if _, ok := payload.Secrets[name]; !ok {
			return fmt.Errorf("secret %s is used in cluster.yaml but missing", name)
		}
	}
	s.Config = ClusterConfig{Revision: s.Config.Revision + 1, Hash: payload.Hash, Spec: payload.Spec.WithDefaults()}
	s.Secrets = payload.Secrets
	s.JoinKeys = payload.JoinKeys
	s.Registries = payload.Registries
	return nil
}

// CreateStack registers a stack before its first apply.
type CreateStack struct {
	Name  string `json:"name"`
	Owner string `json:"owner"`
}

func applyCreateStack(s *State, cmd Command, payload CreateStack) error {
	switch {
	case !names.ValidStack(payload.Name):
		return fmt.Errorf("stack name %q: use lowercase letters, digits, - and _", payload.Name)
	case payload.Owner == "":
		return errors.New("the stack needs an owner")
	}
	if _, exists := s.Stacks[payload.Name]; exists {
		return fmt.Errorf("stack %s exists already", payload.Name)
	}
	createdBy := cmd.Actor
	if IsUserActor(cmd.Actor) {
		createdBy = strings.TrimPrefix(cmd.Actor, userActorPrefix)
	}
	s.Stacks[payload.Name] = &Stack{
		Name: payload.Name, Owner: payload.Owner, CreatedBy: createdBy, Status: StackCreated,
	}
	return nil
}

// PutStack replaces the Compose file of a registered stack; the agent of its
// host applies the new generation.
type PutStack struct {
	Name     string `json:"name"`
	Owner    string `json:"owner"`
	Source   string `json:"source"`
	Priority int    `json:"priority"`
	// Secrets replace the stack's secrets as a whole: every sender sends all
	// the stack asks for, an operation too. So whoever cannot decrypt the
	// stack's secrets file can only remove the secrets, never keep them for a
	// changed stack.
	Secrets map[string]Sealed `json:"secrets,omitempty"`
	// Operation is set when the operation holding the lock makes the change.
	Operation string `json:"operation,omitempty"`
}

// The limits of a stack's secrets: every apply writes them into the Raft
// log. The API checks the values' sizes before it seals them; the state can
// check only the count, as it sees them sealed.
const (
	MaxStackSecrets      = 64
	MaxSecretBytes       = 64 << 10
	MaxStackSecretsBytes = 256 << 10
)

func applyPutStack(s *State, _ Command, payload PutStack) error {
	switch {
	case !names.ValidStack(payload.Name):
		return fmt.Errorf("stack name %q: use lowercase letters, digits, - and _", payload.Name)
	case payload.Owner == "":
		return errors.New("the stack needs an owner")
	case payload.Source == "":
		return fmt.Errorf("stack %s has no Compose file", payload.Name)
	case len(payload.Secrets) > MaxStackSecrets:
		return fmt.Errorf("stack %s has %d secrets, more than %d", payload.Name, len(payload.Secrets), MaxStackSecrets)
	}
	for _, secret := range slices.Sorted(maps.Keys(payload.Secrets)) {
		if !names.ValidSecret(secret) {
			return fmt.Errorf("stack %s: %q is not a secret name", payload.Name, secret)
		}
	}
	if err := s.checkUnlocked(stackLock(payload.Name), payload.Operation); err != nil {
		return err
	}
	stack, exists := s.Stacks[payload.Name]
	switch {
	case !exists:
		return fmt.Errorf("no stack %s; register it first (clowder stack create %s)", payload.Name, payload.Name)
	case stack.Owner != payload.Owner:
		return fmt.Errorf("stack %s belongs to %s, not to %s", stack.Name, stack.Owner, payload.Owner)
	}
	if stack.Status == StackCreated {
		stack.Status = StackPending // the only way out of created
	}
	stack.Generation++
	stack.Source = payload.Source
	stack.Priority = payload.Priority
	stack.Secrets = payload.Secrets
	return nil
}

// DeleteStack removes a stack. A stack with volumes goes only together with
// them (WithVolumes), in this one command: deleted one by one, the
// placement would put a volume back between two commands.
type DeleteStack struct {
	Name        string `json:"name"`
	WithVolumes bool   `json:"with_volumes,omitempty"`
	// Operation is set when the operation holding the lock makes the change.
	Operation string `json:"operation,omitempty"`
}

func applyDeleteStack(s *State, _ Command, payload DeleteStack) error {
	if _, ok := s.Stacks[payload.Name]; !ok {
		return fmt.Errorf("no stack %s", payload.Name)
	}
	if err := s.checkUnlocked(stackLock(payload.Name), payload.Operation); err != nil {
		return err
	}
	var volumes []string
	for _, id := range slices.Sorted(maps.Keys(s.Volumes)) {
		if !strings.HasPrefix(id, payload.Name+"/") {
			continue
		}
		if !payload.WithVolumes {
			return fmt.Errorf("stack %s still has volume %s", payload.Name, id)
		}
		if err := s.checkUnlocked(volumeLock(id), payload.Operation); err != nil {
			return err
		}
		volumes = append(volumes, id)
	}
	for _, id := range volumes {
		delete(s.Volumes, id)
	}
	delete(s.Stacks, payload.Name)
	return nil
}

// SetStackStatus records where a stack runs and how it is doing.
type SetStackStatus struct {
	Name    string      `json:"name"`
	Status  StackStatus `json:"status"`
	Host    *HostID     `json:"host,omitempty"`
	Stopped []string    `json:"stopped,omitempty"`
}

func applySetStackStatus(s *State, cmd Command, payload SetStackStatus) error {
	stack, ok := s.Stacks[payload.Name]
	switch {
	case !ok:
		return fmt.Errorf("no stack %s", payload.Name)
	case stack.Status == StackCreated:
		return fmt.Errorf("stack %s was never applied", payload.Name)
	}
	switch payload.Status {
	case StackPending, StackStarting, StackRunning, StackWaitingForHost, StackStopped:
	default:
		return fmt.Errorf("stack %s: unknown status %q", payload.Name, payload.Status)
	}
	if err := s.checkLiveHost(payload.Host); err != nil {
		return err
	}
	// a host reports only about a stack it runs, and never moves it: the
	// leader may have moved it since the host read the state
	if IsHostActor(cmd.Actor) && (stack.Host == nil || cmd.Actor != HostActor(*stack.Host) ||
		!samePlace(stack.Host, payload.Host)) {
		return fmt.Errorf("stack %s: only its host reports about it, and only where it is", payload.Name)
	}
	if !samePlace(stack.Host, payload.Host) {
		stack.LastMove = cmd.At
	}
	stack.Status, stack.Host, stack.Stopped = payload.Status, payload.Host, payload.Stopped
	return nil
}

func samePlace(a, b *HostID) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

// checkLiveHost accepts no host or one that is a member.
func (s *State) checkLiveHost(id *HostID) error {
	if id == nil {
		return nil
	}
	if host, ok := s.Hosts[*id]; !ok || host.Phase == Removed {
		return fmt.Errorf("host %d is not a member", *id)
	}
	return nil
}

// MaxMinor is the highest DRBD minor: its port 10000 + minor stays in the
// range compose.ReservedPort keeps for DRBD (10000-10999); both change
// together.
const MaxMinor = 999

// PutVolume creates a volume or replaces its record.
type PutVolume struct {
	Volume Volume `json:"volume"`
	// Operation is set when the operation holding the lock makes the change.
	Operation string `json:"operation,omitempty"`
}

func applyPutVolume(s *State, _ Command, payload PutVolume) error {
	volume := payload.Volume
	stackName, _, ok := strings.Cut(volume.ID, "/")
	if !ok {
		return fmt.Errorf("volume ID %q is not stack/volume", volume.ID)
	}
	if _, ok := s.Stacks[stackName]; !ok {
		return fmt.Errorf("volume %s: no stack %s", volume.ID, stackName)
	}
	if err := s.checkUnlocked(volumeLock(volume.ID), payload.Operation); err != nil {
		return err
	}
	if err := checkVolume(volume); err != nil {
		return err
	}
	if old, ok := s.Volumes[volume.ID]; ok && volume.Generation < old.Generation {
		return fmt.Errorf("volume %s: generation %d is older than %d", volume.ID, volume.Generation, old.Generation)
	}
	if volume.Minor != nil {
		if *volume.Minor < 0 || *volume.Minor > MaxMinor {
			return fmt.Errorf("volume %s: minor %d is not from 0 to %d", volume.ID, *volume.Minor, MaxMinor)
		}
		for _, other := range s.Volumes {
			if other.ID != volume.ID && other.Minor != nil && *other.Minor == *volume.Minor {
				return fmt.Errorf("volume %s: minor %d belongs to %s", volume.ID, *volume.Minor, other.ID)
			}
		}
	}
	// fewer placements than replicas is allowed: a degraded volume waits for
	// its missing copies
	placed := map[HostID]bool{}
	for _, placement := range volume.Placements {
		if err := s.checkLiveHost(&placement.Host); err != nil {
			return fmt.Errorf("volume %s: %w", volume.ID, err)
		}
		if placed[placement.Host] {
			return fmt.Errorf("volume %s: two copies on host %d", volume.ID, placement.Host)
		}
		placed[placement.Host] = true
	}
	s.Volumes[volume.ID] = &volume
	return nil
}

// checkVolume holds what a volume's record must be on its own: the state is
// the last place to catch a wrong one.
func checkVolume(volume Volume) error {
	copies := 0
	for _, placement := range volume.Placements {
		if !placement.Tiebreaker {
			copies++
		}
	}
	switch {
	case volume.Replicas < 0 || volume.Replicas > 3:
		return fmt.Errorf("volume %s: %d replicas, not 0 to 3", volume.ID, volume.Replicas)
	case volume.Class == "" || volume.SizeBytes <= 0:
		return fmt.Errorf("volume %s needs a class and a size", volume.ID)
	case copies > max(volume.Replicas, 1):
		// replicas 0 is one plain LV on one host
		return fmt.Errorf("volume %s: %d copies for %d replicas", volume.ID, copies, volume.Replicas)
	case (volume.Replicas == 0) != (volume.Minor == nil):
		return fmt.Errorf("volume %s: a DRBD minor exactly when it has replicas", volume.ID)
	}
	switch volume.Status {
	case VolumeOK, VolumeDegraded, VolumeResyncing, VolumeRestorePending:
		return nil
	default:
		return fmt.Errorf("volume %s: unknown status %q", volume.ID, volume.Status)
	}
}

// DeleteVolume removes a volume's record.
type DeleteVolume struct {
	ID string `json:"id"`
	// Operation is set when the operation holding the lock makes the change.
	Operation string `json:"operation,omitempty"`
}

func applyDeleteVolume(s *State, _ Command, payload DeleteVolume) error {
	if _, ok := s.Volumes[payload.ID]; !ok {
		return fmt.Errorf("no volume %s", payload.ID)
	}
	if err := s.checkUnlocked(volumeLock(payload.ID), payload.Operation); err != nil {
		return err
	}
	delete(s.Volumes, payload.ID)
	return nil
}
