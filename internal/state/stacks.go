package state

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/webcodingcat/clowder/internal/names"
)

// CreateStack registers a stack before its first apply.
type CreateStack struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Owner     string `json:"owner"`
}

func applyCreateStack(s *State, cmd Command, payload CreateStack) error {
	if err := s.checkStackName(payload.Namespace, payload.Name); err != nil {
		return err
	}
	if payload.Owner == "" {
		return errors.New("the stack needs an owner")
	}
	id := names.ID(payload.Namespace, payload.Name)
	if _, exists := s.Stacks[id]; exists {
		return fmt.Errorf("stack %s exists already", id)
	}
	// a new volume of the same name would have the same LV as a retired one
	// of the deleted stack, and its host would take the old data for it
	if slices.ContainsFunc(s.Retired, func(c RetiredCopy) bool { return stackOfVolume(c.Volume) == id }) {
		return fmt.Errorf("the volumes of the deleted stack %s are still being removed; try again in a minute", id)
	}
	createdBy := cmd.Actor
	if IsUserActor(cmd.Actor) {
		createdBy = strings.TrimPrefix(cmd.Actor, userActorPrefix)
	}
	s.Stacks[id] = &Stack{
		Namespace: payload.Namespace, Name: payload.Name, Owner: payload.Owner, CreatedBy: createdBy,
		Status: StackCreated,
	}
	return nil
}

// stackOfVolume is the ID of the stack a volume belongs to, or "" for an ID
// that names none (put-volume refuses those).
func stackOfVolume(volumeID string) string {
	stackID, _ := names.StackOfVolume(volumeID)
	return stackID
}

// checkStackName refuses a stack name that is not valid, or a namespace the
// cluster does not have.
func (s *State) checkStackName(namespace, stack string) error {
	if !names.Valid(stack) {
		return fmt.Errorf("stack name %q: %s", stack, names.Rule)
	}
	if _, ok := s.Namespaces[namespace]; !ok {
		return fmt.Errorf("no namespace %q", namespace)
	}
	return nil
}

// PutStack replaces the Compose file of a registered stack; the agent of its
// host applies the new generation.
type PutStack struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Owner     string `json:"owner"`
	Source    string `json:"source"`
	Priority  int    `json:"priority"`
	// Ports and Routes are what the stack publishes, as the API read them
	// from Source (compose.Stack.PublishedPorts and Routes): the state does
	// not parse Compose files, but must refuse what another stack has.
	Ports  []string `json:"ports,omitempty"`
	Routes []string `json:"routes,omitempty"`
	// Services are the stack's services as the scheduler needs them.
	Services map[string]Service `json:"services,omitempty"`
	// Privileged is true when a service asks for what reaches beyond its
	// container: the state, the last place to check, refuses it unless
	// stacks.privileged names the stack.
	Privileged bool `json:"privileged,omitempty"`
	// Secrets replace the stack's secrets as a whole: every sender sends all
	// the stack asks for, an operation too. So whoever cannot decrypt the
	// stack's secrets file can only remove the secrets, never keep them for a
	// changed stack.
	Secrets map[string]Sealed `json:"secrets,omitempty"`
	// ConfigRevision is the cluster.yaml the stack was checked against
	// (storage classes, priorities, privileges): if a cluster apply came in
	// between, the check no longer holds and the command is refused.
	ConfigRevision int64 `json:"config_revision"`
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
	if err := s.checkStackName(payload.Namespace, payload.Name); err != nil {
		return err
	}
	id := names.ID(payload.Namespace, payload.Name)
	switch {
	case payload.Owner == "":
		return errors.New("the stack needs an owner")
	case payload.Source == "":
		return fmt.Errorf("stack %s has no Compose file", id)
	case len(payload.Secrets) > MaxStackSecrets:
		return fmt.Errorf("stack %s has %d secrets, more than %d", id, len(payload.Secrets), MaxStackSecrets)
	case payload.ConfigRevision != s.Config.Revision:
		return fmt.Errorf("stack %s was checked against cluster.yaml revision %d, but %d came in between; apply again",
			id, payload.ConfigRevision, s.Config.Revision)
	case payload.Privileged && !slices.Contains(s.Config.Spec.Stacks.Privileged, id):
		return fmt.Errorf("stack %s reaches beyond its containers; only stacks named in stacks.privileged of "+
			"cluster.yaml may", id)
	}
	for _, secret := range slices.Sorted(maps.Keys(payload.Secrets)) {
		if !names.Valid(secret) {
			return fmt.Errorf("stack %s: %q is not a secret name", id, secret)
		}
	}
	if err := s.checkUnlocked(stackLock(id), payload.Operation); err != nil {
		return err
	}
	if err := s.checkUnclaimed(id, payload); err != nil {
		return err
	}
	for _, name := range slices.Sorted(maps.Keys(payload.Services)) {
		for _, dependency := range payload.Services[name].DependsOn {
			if _, ok := payload.Services[dependency]; !ok {
				return fmt.Errorf("stack %s: service %s depends on %s, which the stack does not have",
					id, name, dependency)
			}
		}
	}
	stack, exists := s.Stacks[id]
	switch {
	case !exists:
		return fmt.Errorf("no stack %s; register it first (clowder stack create)", id)
	case stack.Owner != payload.Owner:
		return fmt.Errorf("stack %s belongs to %s, not to %s", id, stack.Owner, payload.Owner)
	}
	switch stack.Status {
	case StackCreated:
		stack.Status = StackPending // the only way out of created
	case StackRunning:
		// the host reports running again once the new generation is up, so
		// users can tell when it is
		stack.Status = StackStarting
	}
	stack.Generation++
	stack.Source = payload.Source
	stack.Priority = payload.Priority
	stack.Ports, stack.Routes = payload.Ports, payload.Routes
	stack.Services = payload.Services
	stack.Privileged = payload.Privileged
	// a service the new generation no longer has is pushed out no more; the
	// scheduler looks at the stack again at its next pass
	stack.PushedOut = slices.DeleteFunc(stack.PushedOut, func(service string) bool {
		_, kept := payload.Services[service]
		return !kept
	})
	stack.Secrets = payload.Secrets
	s.StacksRevision++
	return nil
}

// checkUnclaimed refuses a port or an HTTP route that another stack has:
// both stacks would reach the same address. A stack's own earlier
// generation does not count, so an apply keeps what it had.
func (s *State) checkUnclaimed(id string, payload PutStack) error {
	for _, otherID := range slices.Sorted(maps.Keys(s.Stacks)) {
		if otherID == id {
			continue
		}
		other := s.Stacks[otherID]
		for _, port := range payload.Ports {
			if slices.Contains(other.Ports, port) {
				return fmt.Errorf("stack %s: port %s is published by stack %s already", id, port, otherID)
			}
		}
		for _, route := range payload.Routes {
			if slices.Contains(other.Routes, route) {
				return fmt.Errorf("stack %s: route %s belongs to stack %s already", id, route, otherID)
			}
		}
	}
	return nil
}

// DeleteStack removes a stack. A stack with volumes goes only together with
// them (WithVolumes), in this one command: deleted one by one, the
// placement would put a volume back between two commands.
type DeleteStack struct {
	Namespace   string `json:"namespace"`
	Name        string `json:"name"`
	WithVolumes bool   `json:"with_volumes,omitempty"`
	// Operation is set when the operation holding the lock makes the change.
	Operation string `json:"operation,omitempty"`
}

func applyDeleteStack(s *State, _ Command, payload DeleteStack) error {
	id := names.ID(payload.Namespace, payload.Name)
	if _, ok := s.Stacks[id]; !ok {
		return fmt.Errorf("no stack %s", id)
	}
	if err := s.checkUnlocked(stackLock(id), payload.Operation); err != nil {
		return err
	}
	var volumes []string
	for _, volumeID := range slices.Sorted(maps.Keys(s.Volumes)) {
		if stackOfVolume(volumeID) != id {
			continue
		}
		if !payload.WithVolumes {
			return fmt.Errorf("stack %s still has volume %s", id, volumeID)
		}
		if err := s.checkUnlocked(volumeLock(volumeID), payload.Operation); err != nil {
			return err
		}
		volumes = append(volumes, volumeID)
	}
	for _, volumeID := range volumes {
		s.retire(s.Volumes[volumeID], nil)
		delete(s.Volumes, volumeID)
	}
	delete(s.Stacks, id)
	return nil
}

// SetStackStatus records where a stack runs and how it is doing.
type SetStackStatus struct {
	ID     string      `json:"id"` // the stack's
	Status StackStatus `json:"status"`
	Host   *HostID     `json:"host,omitempty"`
	// Generation is the one the host ran when it reports; the leader leaves
	// it 0. A report about an older generation is refused: an apply came in
	// between, and that generation has yet to come up.
	Generation int64 `json:"generation,omitempty"`
}

func applySetStackStatus(s *State, cmd Command, payload SetStackStatus) error {
	stack, ok := s.Stacks[payload.ID]
	switch {
	case !ok:
		return fmt.Errorf("no stack %s", payload.ID)
	case stack.Status == StackCreated:
		return fmt.Errorf("stack %s was never applied", payload.ID)
	}
	switch payload.Status {
	case StackPending, StackStarting, StackRunning, StackWaitingForHost, StackStopped:
	default:
		return fmt.Errorf("stack %s: unknown status %q", payload.ID, payload.Status)
	}
	if err := s.checkLiveHost(payload.Host); err != nil {
		return err
	}
	// a host reports only about a stack it runs, and never moves it: the
	// leader may have moved it since the host read the state
	if IsHostActor(cmd.Actor) && (stack.Host == nil || cmd.Actor != HostActor(*stack.Host) ||
		!samePlace(stack.Host, payload.Host)) {
		return fmt.Errorf("stack %s: only its host reports about it, and only where it is", payload.ID)
	}
	if IsHostActor(cmd.Actor) && payload.Generation != stack.Generation {
		return fmt.Errorf("stack %s: the report is about generation %d, but %d is applied",
			payload.ID, payload.Generation, stack.Generation)
	}
	if !samePlace(stack.Host, payload.Host) {
		stack.LastMove = cmd.At
	}
	stack.Status, stack.Host = payload.Status, payload.Host
	return nil
}

// SetPushedOut is one decision of the scheduler about pushed-out services:
// the whole new list of each stack it touches (empty: none). It either
// pushes out, possibly in several stacks, or brings one service back.
type SetPushedOut struct {
	Stacks map[string][]string `json:"stacks"` // by the stack's ID
}

func applySetPushedOut(s *State, _ Command, payload SetPushedOut) error {
	for _, id := range slices.Sorted(maps.Keys(payload.Stacks)) {
		stack, ok := s.Stacks[id]
		if !ok {
			return fmt.Errorf("no stack %s", id)
		}
		for _, service := range payload.Stacks[id] {
			if _, ok := stack.Services[service]; !ok {
				return fmt.Errorf("stack %s has no service %s", id, service)
			}
		}
	}
	for id, services := range payload.Stacks {
		s.Stacks[id].PushedOut = slices.Compact(slices.Sorted(slices.Values(services)))
	}
	return nil
}

// SetStackWaitReason records why the placement cannot place a stack or a
// volume it newly names; an empty reason clears it. It changes nothing else,
// so it never moves or stops a stack that runs. WaitingSince starts with the
// first reason, at the command's time.
type SetStackWaitReason struct {
	ID     string `json:"id"` // the stack's
	Reason string `json:"reason,omitempty"`
}

func applySetStackWaitReason(s *State, cmd Command, payload SetStackWaitReason) error {
	stack, ok := s.Stacks[payload.ID]
	if !ok {
		return fmt.Errorf("no stack %s", payload.ID)
	}
	switch {
	case payload.Reason == "":
		stack.WaitingSince = time.Time{}
	case stack.WaitReason == "":
		stack.WaitingSince = cmd.At
	}
	stack.WaitReason = payload.Reason
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
// range stackspec.ReservedPort keeps for DRBD (10000-10999); both change
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
	stackID, ok := names.StackOfVolume(volume.ID)
	if !ok {
		return fmt.Errorf("volume ID %q is not namespace/stack/volume", volume.ID)
	}
	if _, ok := s.Stacks[stackID]; !ok {
		return fmt.Errorf("volume %s: no stack %s", volume.ID, stackID)
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
	if old, ok := s.Volumes[volume.ID]; ok {
		s.retire(old, &volume)
	}
	s.Volumes[volume.ID] = &volume
	return nil
}

// retire records the copies of old as retired, except those that kept,
// the volume's new record, still has: same host, same generation.
func (s *State) retire(old *Volume, kept *Volume) {
	for _, placement := range old.Placements {
		if kept != nil && kept.Generation == old.Generation &&
			slices.ContainsFunc(kept.Placements, func(p Placement) bool { return p.Host == placement.Host }) {
			continue
		}
		retired := RetiredCopy{Volume: old.ID, Generation: old.Generation, Host: placement.Host}
		if !slices.Contains(s.Retired, retired) {
			s.Retired = append(s.Retired, retired)
		}
	}
}

// ForgetCopy is a host telling that a retired copy of its is gone.
type ForgetCopy struct {
	Copy RetiredCopy `json:"copy"`
}

func applyForgetCopy(s *State, cmd Command, payload ForgetCopy) error {
	if cmd.Actor != HostActor(payload.Copy.Host) {
		return fmt.Errorf("only host %d itself forgets its copies", payload.Copy.Host)
	}
	index := slices.Index(s.Retired, payload.Copy)
	if index < 0 {
		return fmt.Errorf("no retired copy of %s (generation %d) on host %d",
			payload.Copy.Volume, payload.Copy.Generation, payload.Copy.Host)
	}
	s.Retired = slices.Delete(s.Retired, index, index+1)
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
	case volume.Generation < 0 || volume.Generation > names.MaxDataGeneration:
		// the generation is part of the LV's name, whose length has a limit
		return fmt.Errorf("volume %s: generation %d is not from 0 to %d", volume.ID, volume.Generation,
			names.MaxDataGeneration)
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
	s.retire(s.Volumes[payload.ID], nil)
	delete(s.Volumes, payload.ID)
	return nil
}
