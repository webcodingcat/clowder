package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/webcodingcat/clowder/internal/clusterdir"
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
	s.Config = ClusterConfig{Revision: s.Config.Revision + 1, Hash: payload.Hash, Spec: payload.Spec}
	s.Secrets = payload.Secrets
	s.JoinKeys = payload.JoinKeys
	s.Registries = payload.Registries
	return nil
}

// PutStack creates a stack or replaces its spec.
type PutStack struct {
	Name     string          `json:"name"`
	Owner    string          `json:"owner"`
	Spec     json.RawMessage `json:"spec"`
	Priority int             `json:"priority"`
}

func applyPutStack(s *State, _ Command, payload PutStack) error {
	switch {
	case payload.Name == "" || payload.Owner == "":
		return errors.New("the stack needs a name and an owner")
	case len(payload.Spec) == 0:
		return fmt.Errorf("stack %s has no spec", payload.Name)
	}
	if err := s.checkUnlocked(stackLock(payload.Name)); err != nil {
		return err
	}
	stack, exists := s.Stacks[payload.Name]
	if !exists {
		stack = &Stack{Name: payload.Name, Owner: payload.Owner, Status: StackPending}
		s.Stacks[payload.Name] = stack
	} else if stack.Owner != payload.Owner {
		return fmt.Errorf("stack %s belongs to %s, not to %s", stack.Name, stack.Owner, payload.Owner)
	}
	stack.Generation++
	stack.Spec = payload.Spec
	stack.Priority = payload.Priority
	return nil
}

// DeleteStack removes a stack; its volumes go with their own commands.
type DeleteStack struct {
	Name string `json:"name"`
}

func applyDeleteStack(s *State, _ Command, payload DeleteStack) error {
	if _, ok := s.Stacks[payload.Name]; !ok {
		return fmt.Errorf("no stack %s", payload.Name)
	}
	if err := s.checkUnlocked(stackLock(payload.Name)); err != nil {
		return err
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
	if !ok {
		return fmt.Errorf("no stack %s", payload.Name)
	}
	if err := s.checkLiveHost(payload.Host); err != nil {
		return err
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

// maxMinor is the highest DRBD minor: its port 10000 + minor stays in the
// range clowder reserves for DRBD.
const maxMinor = 999

// PutVolume creates a volume or replaces its record.
type PutVolume struct {
	Volume Volume `json:"volume"`
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
	if err := s.checkUnlocked(volumeLock(volume.ID)); err != nil {
		return err
	}
	if old, ok := s.Volumes[volume.ID]; ok && volume.Generation < old.Generation {
		return fmt.Errorf("volume %s: generation %d is older than %d", volume.ID, volume.Generation, old.Generation)
	}
	if volume.Minor != nil {
		if *volume.Minor < 0 || *volume.Minor > maxMinor {
			return fmt.Errorf("volume %s: minor %d is not from 0 to %d", volume.ID, *volume.Minor, maxMinor)
		}
		for _, other := range s.Volumes {
			if other.ID != volume.ID && other.Minor != nil && *other.Minor == *volume.Minor {
				return fmt.Errorf("volume %s: minor %d belongs to %s", volume.ID, *volume.Minor, other.ID)
			}
		}
	}
	for _, placement := range volume.Placements {
		if err := s.checkLiveHost(&placement.Host); err != nil {
			return fmt.Errorf("volume %s: %w", volume.ID, err)
		}
	}
	s.Volumes[volume.ID] = &volume
	return nil
}

// DeleteVolume removes a volume's record.
type DeleteVolume struct {
	ID string `json:"id"`
}

func applyDeleteVolume(s *State, _ Command, payload DeleteVolume) error {
	if _, ok := s.Volumes[payload.ID]; !ok {
		return fmt.Errorf("no volume %s", payload.ID)
	}
	if err := s.checkUnlocked(volumeLock(payload.ID)); err != nil {
		return err
	}
	delete(s.Volumes, payload.ID)
	return nil
}
