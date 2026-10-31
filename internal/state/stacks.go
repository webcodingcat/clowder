package state

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/webcodingcat/clowder/internal/clusterdir"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/resource"
	"github.com/webcodingcat/clowder/internal/stackspec"
	"github.com/webcodingcat/clowder/internal/units"
)

// MaxStackSecrets is how many secrets a stack may name: each is a file or
// a variable on its host, rewritten at every change.
const MaxStackSecrets = 64

func findStack(s *State, id string) *Meta {
	if stack, ok := s.Stacks[id]; ok {
		return &stack.Meta
	}
	return nil
}

// putStack stores a stack's spec, normalized, so a spec written another way
// but meaning the same is unchanged. The state reads the spec itself: it
// checks it against the cluster (priority categories, privileges, ports
// and routes of other stacks) and the namespace (its volumes and secrets),
// and works out what the scheduler needs. A running stack becomes
// starting until its host reports the new generation up; any other keeps
// its status, which its host or the placement moves on.
func putStack(s *State, id string, payload PutResource) (*Meta, error) {
	written, err := decodeSpec[stackspec.Spec](payload.Kind, id, payload.Spec)
	if err != nil {
		return nil, err
	}
	spec := stackspec.Normalize(written)
	stored, exists := s.Stacks[id]
	if exists && sameJSON(stored.Spec, spec) {
		return nil, errUnchanged
	}
	if problems := stackspec.Check(spec); len(problems) > 0 {
		return nil, fmt.Errorf("stack %s: %w", id, problems)
	}
	services, err := servicesOf(spec, s.Config.Spec.WithDefaults().Priorities)
	if err != nil {
		return nil, fmt.Errorf("stack %s: %w", id, err)
	}
	var host *HostID
	if exists {
		host = stored.Host
	}
	if err := s.checkStack(id, payload.Namespace, host, spec); err != nil {
		return nil, err
	}
	if err := s.checkUnlocked(stackLock(id), ""); err != nil {
		return nil, err
	}
	switch {
	case !exists:
		stored = &Stack{
			Meta:   Meta{Namespace: payload.Namespace, Name: payload.Name, Owner: payload.Owner},
			Status: StackPending,
		}
		s.Stacks[id] = stored
	case stored.Status == StackRunning:
		// the host reports running again once the new generation is up, so
		// users can tell when it is
		stored.Status = StackStarting
	}
	stored.Spec, stored.Services = spec, services
	// a service the new generation no longer has is pushed out no more; the
	// scheduler looks at the stack again at its next pass
	stored.PushedOut = slices.DeleteFunc(stored.PushedOut, func(service string) bool {
		_, kept := services[service]
		return !kept
	})
	maps.DeleteFunc(stored.Restarted, func(service string, _ time.Time) bool {
		_, kept := services[service]
		return !kept
	})
	return &stored.Meta, nil
}

// RestartStack recreates services of a stack whose spec has not changed: a
// new image under the same tag, a retry. It names services of the stack, or
// none for all of them; those that depend on them come along.
type RestartStack struct {
	Namespace string   `json:"namespace"`
	Name      string   `json:"name"`
	Services  []string `json:"services,omitempty"`
}

// applyRestartStack records the restart's time for each service. It changes
// no spec, so the generation stays; and any channel may restart a stack,
// whoever owns it, so an operator can restart one that GitOps applies.
// Whether a user may is Authorize's. A running stack is starting until its
// host reports the recreated containers up.
func applyRestartStack(s *State, cmd Command, payload RestartStack) error {
	if err := Authorize(cmd.Actor, VerbRestart, resource.KindStack, payload.Namespace); err != nil {
		return err
	}
	id := names.ID(payload.Namespace, payload.Name)
	stack, ok := s.Stacks[id]
	if !ok {
		return fmt.Errorf("no stack %s", id)
	}
	for _, service := range payload.Services {
		if _, ok := stack.Spec.Services[service]; !ok {
			return fmt.Errorf("stack %s has no service %s", id, service)
		}
	}
	if err := s.checkUnlocked(stackLock(id), ""); err != nil {
		return err
	}
	named := payload.Services
	if len(named) == 0 {
		named = slices.Collect(maps.Keys(stack.Spec.Services))
	}
	if stack.Restarted == nil {
		stack.Restarted = map[string]time.Time{}
	}
	for _, service := range stack.Spec.WithDependents(named) {
		stack.Restarted[service] = cmd.At
	}
	if stack.Status == StackRunning {
		stack.Status = StackStarting
	}
	s.changed(resource.KindStack, &stack.Meta)
	return nil
}

// servicesOf works out what the scheduler needs of each service: its
// priority as written (its own, else the stack's), which must resolve with
// the cluster's categories, its reservations (zero for the cluster's
// default) and what it depends on.
func servicesOf(spec stackspec.Spec, priorities clusterdir.Priorities) (map[string]Service, error) {
	services := map[string]Service{}
	for _, name := range slices.Sorted(maps.Keys(spec.Services)) {
		service := spec.Services[name]
		priority, err := stackspec.ParsePriority(cmp.Or(service.Priority, spec.Priority))
		if err != nil {
			return nil, fmt.Errorf("service %s: %w", name, err)
		}
		if _, err := priorities.Resolve(priority); err != nil {
			return nil, fmt.Errorf("service %s: %w", name, err)
		}
		reserved := Service{Priority: priority, DependsOn: slices.Clone(service.DependsOn)}
		if reserve := service.Resources.Reserve; reserve != nil {
			reserved.CPUs = reserve.CPUs
			if reserve.Memory != "" {
				memory, err := units.ParseSize(reserve.Memory)
				if err != nil {
					return nil, fmt.Errorf("service %s: %w", name, err)
				}
				reserved.MemoryBytes = memory
			}
		}
		services[name] = reserved
	}
	return services, nil
}

// checkStack checks a stack against what only the state knows: privileges
// need stacks.privileged; no other stack may publish its ports or take its
// routes (both would reach the same address); its volumes and secrets must
// be its namespace's, and a volume serves one stack only, as two stacks on
// one file system would ruin it. A placed stack (host) cannot name a volume
// whose data is on another host: it could never start.
func (s *State) checkStack(id, namespace string, host *HostID, spec stackspec.Spec) error {
	if spec.Privileged() && !slices.Contains(s.Config.Spec.Stacks.Privileged, id) {
		return fmt.Errorf("stack %s reaches beyond its containers; only stacks named in stacks.privileged of "+
			"cluster.yaml may", id)
	}
	ports, routes := spec.PublishedPorts(), spec.Routes()
	for _, otherID := range slices.Sorted(maps.Keys(s.Stacks)) {
		if otherID == id {
			continue
		}
		other := s.Stacks[otherID]
		for _, port := range other.Spec.PublishedPorts() {
			if slices.Contains(ports, port) {
				return fmt.Errorf("stack %s: port %s is published by stack %s already", id, port, otherID)
			}
		}
		for _, route := range other.Spec.Routes() {
			if slices.Contains(routes, route) {
				return fmt.Errorf("stack %s: route %s belongs to stack %s already", id, route, otherID)
			}
		}
	}
	for _, volume := range spec.VolumeNames() {
		named, ok := s.Volumes[names.ID(namespace, volume)]
		if !ok {
			return fmt.Errorf("stack %s: namespace %s has no volume %s", id, namespace, volume)
		}
		if host != nil && named.Status != VolumePending && !slices.ContainsFunc(named.Placements,
			func(placement Placement) bool { return placement.Host == *host && !placement.Tiebreaker }) {
			return fmt.Errorf("stack %s runs on host %d, but volume %s has its data on another host", id, *host, volume)
		}
		if other := s.stackNaming(namespace, volume, id, func(stack *Stack) []string {
			return stack.Spec.VolumeNames()
		}); other != "" {
			return fmt.Errorf("stack %s: volume %s is stack %s's already; a volume serves one stack", id, volume, other)
		}
	}
	secrets := spec.SecretNames()
	if len(secrets) > MaxStackSecrets {
		return fmt.Errorf("stack %s names %d secrets, more than %d", id, len(secrets), MaxStackSecrets)
	}
	for _, secret := range secrets {
		if _, ok := s.Secrets[names.ID(namespace, secret)]; !ok {
			return fmt.Errorf("stack %s: namespace %s has no secret %s", id, namespace, secret)
		}
	}
	return nil
}

// removeStack removes a stack; its volumes stay, with their data, for the
// next stack that names them.
func removeStack(s *State, id string) error {
	if err := s.checkUnlocked(stackLock(id), ""); err != nil {
		return err
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
	// AppliedSecrets are the digests of the secrets the containers came up
	// with, by name; the host sends them with running, the leader none.
	AppliedSecrets map[string]string `json:"applied_secrets,omitempty"`
}

func applySetStackStatus(s *State, cmd Command, payload SetStackStatus) error {
	stack, ok := s.Stacks[payload.ID]
	if !ok {
		return fmt.Errorf("no stack %s", payload.ID)
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
	applied := stack.AppliedSecrets
	if IsHostActor(cmd.Actor) {
		applied = payload.AppliedSecrets
	}
	if stack.Status == payload.Status && samePlace(stack.Host, payload.Host) && maps.Equal(stack.AppliedSecrets, applied) {
		return errUnchanged
	}
	if !samePlace(stack.Host, payload.Host) {
		stack.LastMove = cmd.At
	}
	stack.Status, stack.Host, stack.AppliedSecrets = payload.Status, payload.Host, applied
	s.changed(resource.KindStack, &stack.Meta)
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
	unchanged := true
	for _, id := range slices.Sorted(maps.Keys(payload.Stacks)) {
		stack := s.Stacks[id]
		pushedOut := slices.Compact(slices.Sorted(slices.Values(payload.Stacks[id])))
		if !slices.Equal(stack.PushedOut, pushedOut) {
			stack.PushedOut, unchanged = pushedOut, false
			s.changed(resource.KindStack, &stack.Meta)
		}
	}
	if unchanged {
		return errUnchanged
	}
	return nil
}

// SetStackWaitReason records why the placement cannot place a stack or a
// volume it names; an empty reason clears it. It changes nothing else, so
// it never moves or stops a stack that runs. WaitingSince starts with the
// first reason, at the command's time.
type SetStackWaitReason struct {
	ID     string `json:"id"` // the stack's
	Reason string `json:"reason,omitempty"`
}

func applySetStackWaitReason(s *State, cmd Command, payload SetStackWaitReason) error {
	stack, ok := s.Stacks[payload.ID]
	switch {
	case !ok:
		return fmt.Errorf("no stack %s", payload.ID)
	case stack.WaitReason == payload.Reason:
		return errUnchanged
	case payload.Reason == "":
		stack.WaitingSince = time.Time{}
	case stack.WaitReason == "":
		stack.WaitingSince = cmd.At
	}
	stack.WaitReason = payload.Reason
	s.changed(resource.KindStack, &stack.Meta)
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
