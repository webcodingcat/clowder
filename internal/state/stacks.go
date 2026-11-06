package state

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/webcodingcat/clowder/internal/clusterspec"
	"github.com/webcodingcat/clowder/internal/hostrole"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/resource"
	"github.com/webcodingcat/clowder/internal/stackspec"
	"github.com/webcodingcat/clowder/internal/units"
)

// MaxStackSecrets is how many secrets a stack may name. Each secret is a
// file or a variable on the stack's host, rewritten at every change.
const MaxStackSecrets = 64

func findStack(s *State, id string) *Meta {
	if stack, ok := s.Stacks[id]; ok {
		return &stack.Meta
	}
	return nil
}

// putStack stores a stack's spec in normalized form, so a spec that is
// written differently but means the same counts as unchanged. The state
// reads the spec itself. It checks the spec against the cluster (priority
// categories, privileges, ports and routes of other stacks) and against
// the namespace (its volumes and secrets), and works out what the scheduler
// needs. A running or failing stack becomes starting until its host reports
// that the new generation is up. Any other stack keeps its status, which
// its host or the placement moves on.
func putStack(s *State, id string, payload PutResource) (func() *Meta, error) {
	written, err := decodeSpec[stackspec.Spec](payload.Kind, id, payload.Spec)
	if err != nil {
		return nil, err
	}
	spec := stackspec.Normalize(written)
	stored, exists := s.Stacks[id]
	if exists && sameJSON(stored.Spec, spec) {
		return nil, errUnchanged
	}
	if err := s.checkUnlocked(StackLock(id), 0); err != nil {
		return nil, err
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
	return func() *Meta {
		switch {
		case !exists:
			stored = &Stack{
				Meta:   Meta{Namespace: payload.Namespace, Name: payload.Name, Owner: payload.Owner},
				Status: StackPending,
			}
			s.Stacks[id] = stored
		case stored.Status == StackRunning || stored.Status == StackFailing:
			// the host reports running again once the new generation is up,
			// so users can tell when that is
			stored.Status, stored.UpError = StackStarting, ""
		}
		stored.Spec, stored.Services = spec, services
		// a service the new generation no longer has is no longer pushed out;
		// the scheduler looks at the stack again at its next pass
		stored.PushedOut = slices.DeleteFunc(stored.PushedOut, func(service string) bool {
			_, kept := services[service]
			return !kept
		})
		maps.DeleteFunc(stored.Restarted, func(service string, _ time.Time) bool {
			_, kept := services[service]
			return !kept
		})
		return &stored.Meta
	}, nil
}

// RestartStack recreates services of a stack whose spec has not changed,
// for example for a new image under the same tag, or for a retry. It names
// services of the stack, or none for all of them. The services that depend
// on them are restarted too.
type RestartStack struct {
	Namespace string   `json:"namespace"`
	Name      string   `json:"name"`
	Services  []string `json:"services,omitempty"`
}

// applyRestartStack records the restart's time for each service. It changes
// no spec, so the generation stays the same. Any channel may restart a
// stack, whoever owns it, so an operator can restart a stack that GitOps
// applies. A running or failing stack is starting until its host reports
// that the recreated containers are up.
func applyRestartStack(s *State, cmd Command, payload RestartStack) (func(), error) {
	id := names.ID(payload.Namespace, payload.Name)
	stack, ok := s.Stacks[id]
	if !ok {
		return nil, fmt.Errorf("no stack %s", id)
	}
	if err := s.checkUnlocked(StackLock(id), 0); err != nil {
		return nil, err
	}
	for _, service := range payload.Services {
		if _, ok := stack.Spec.Services[service]; !ok {
			return nil, fmt.Errorf("stack %s has no service %s", id, service)
		}
	}
	named := payload.Services
	if len(named) == 0 {
		named = slices.Collect(maps.Keys(stack.Spec.Services))
	}
	return func() {
		if stack.Restarted == nil {
			stack.Restarted = map[string]time.Time{}
		}
		for _, service := range stack.Spec.WithDependents(named) {
			stack.Restarted[service] = cmd.At
		}
		if stack.Status == StackRunning || stack.Status == StackFailing {
			stack.Status, stack.UpError = StackStarting, ""
		}
		s.changed(resource.KindStack, &stack.Meta)
	}, nil
}

// servicesOf works out what the scheduler needs of each service: its
// priority as written (its own, or else the stack's), which must resolve
// with the cluster's categories; its reservations (zero for the cluster's
// default); and the services it depends on.
func servicesOf(spec stackspec.Spec, priorities clusterspec.Priorities) (map[string]Service, error) {
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

// checkStack checks a stack against what only the state knows:
//   - privileges need an entry in stacks.privileged;
//   - no other stack may publish the same ports or take the same routes,
//     because both would reach the same address;
//   - its volumes and secrets must be in its namespace, and a volume serves
//     only one stack, because two stacks on one file system would ruin it.
//
// A placed stack (host) cannot name a volume whose data is on another host,
// because it could never start.
func (s *State) checkStack(id, namespace string, host *HostID, spec stackspec.Spec) error {
	if spec.Privileged() && !slices.Contains(s.Config.Spec.Stacks.Privileged, id) {
		return fmt.Errorf("stack %s reaches beyond its containers; only stacks named in stacks.privileged of "+
			"the configuration may", id)
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
		if host != nil && named.Status != VolumePending && !slices.ContainsFunc(named.Copies,
			func(volumeCopy VolumeCopy) bool { return volumeCopy.Host == *host && !volumeCopy.Tiebreaker }) {
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
	for _, ref := range spec.SecretKeyRefs() {
		if _, ok := s.Secrets[names.ID(namespace, ref.Name)].Spec.Data[ref.Key]; !ok {
			return fmt.Errorf("stack %s: secret %s has no key %s", id, ref.Name, ref.Key)
		}
	}
	return nil
}

// removeStack removes a stack. Its volumes stay, with their data, for the
// next stack that names them.
func removeStack(s *State, id string) (func(), error) {
	if err := s.checkUnlocked(StackLock(id), 0); err != nil {
		return nil, err
	}
	return func() { delete(s.Stacks, id) }, nil
}

// SetStackStatus records where a stack runs and how it is doing.
type SetStackStatus struct {
	ID     string      `json:"id"` // the stack's
	Status StackStatus `json:"status"`
	Host   *HostID     `json:"host,omitempty"`
	// PlannedAt is the index of the state the leader planned with
	// (checkPlanned); a host leaves it 0.
	PlannedAt uint64 `json:"planned_at,omitempty"`
	// Operation is the leader's operation that makes the change, if any.
	// Only the operation that holds the stack's lock changes the stack, and
	// moves it.
	Operation uint64 `json:"operation,omitempty"`
	// Generation is the generation the host ran when it reports; the leader
	// leaves it 0. A report about an older generation is refused: an apply
	// came in between, and the new generation has yet to come up.
	Generation int64 `json:"generation,omitempty"`
	// Restarted are the restart times the host ran with (Stack.Restarted);
	// the leader sends none. A report about other times is refused, like a
	// report about an older generation, because a restart came in between.
	Restarted map[string]time.Time `json:"restarted,omitempty"`
	// AppliedSecrets are the digests of the secrets the containers came up
	// with, by name. The host sends them with running; the leader sends
	// none.
	AppliedSecrets map[string]string `json:"applied_secrets,omitempty"`
	// UpError is why the host could not bring the stack up. It is set only
	// with failing, which only the host reports.
	UpError string `json:"up_error,omitempty"`
}

func applySetStackStatus(s *State, cmd Command, payload SetStackStatus) (func(), error) {
	stack, ok := s.Stacks[payload.ID]
	if !ok {
		return nil, fmt.Errorf("no stack %s", payload.ID)
	}
	switch payload.Status {
	case StackPending, StackStarting, StackRunning, StackFailing:
	default:
		return nil, fmt.Errorf("stack %s: unknown status %q", payload.ID, payload.Status)
	}
	failing := payload.Status == StackFailing
	switch {
	case failing && !IsHostActor(cmd.Actor):
		return nil, fmt.Errorf("stack %s: only its host reports it failing", payload.ID)
	case failing != (payload.UpError != ""):
		return nil, fmt.Errorf("stack %s: an error goes with failing, and only with it", payload.ID)
	}
	if err := s.checkWorker(payload.Host); err != nil {
		return nil, err
	}
	applied := stack.AppliedSecrets
	if IsHostActor(cmd.Actor) {
		// A host's report ignores locks: it tells what runs where, and an
		// operation that moves the stack waits to hear that.
		// checkHostReport ties it to the stack's place in the state.
		if err := checkHostReport(stack, cmd.Actor, payload); err != nil {
			return nil, err
		}
		// a failing stack's containers of an earlier up may still run
		// with theirs
		if !failing {
			applied = payload.AppliedSecrets
		}
	} else if err := s.checkLeaderStatus(stack, payload); err != nil {
		return nil, err
	}
	if stack.Status == payload.Status && samePlace(stack.Host, payload.Host) &&
		maps.Equal(stack.AppliedSecrets, applied) && stack.UpError == payload.UpError {
		return nil, errUnchanged
	}
	// an operation that gives the stack a host moves it (checkLeaderStatus)
	moved := payload.Operation != 0 && payload.Host != nil && !samePlace(stack.Host, payload.Host)
	return func() {
		stack.Status, stack.Host, stack.AppliedSecrets = payload.Status, payload.Host, applied
		stack.UpError = payload.UpError
		if moved {
			stack.LastMove = cmd.At
		}
		s.changed(resource.KindStack, &stack.Meta)
	}, nil
}

// RequestMove is a user's request to move a stack to another host. The
// placement starts the move (a move-stack operation) once the stack fits
// there.
type RequestMove struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	To        HostID `json:"to"`
}

// applyRequestMove records the request in Stack.MoveTo. It refuses a stack
// that has no host yet (the placement chooses one) and a host that takes
// no stacks. It also refuses while an operation holds the stack, because
// that operation may move it. A move to the host the stack runs on takes
// back an earlier request. Like a restart, it changes no spec, and any
// channel may ask for it.
func applyRequestMove(s *State, cmd Command, payload RequestMove) (func(), error) {
	id := names.ID(payload.Namespace, payload.Name)
	stack, ok := s.Stacks[id]
	if !ok {
		return nil, fmt.Errorf("no stack %s", id)
	}
	if err := s.checkUnlocked(StackLock(id), 0); err != nil {
		return nil, err
	}
	host, ok := s.Hosts[payload.To]
	switch {
	case stack.Host == nil:
		return nil, fmt.Errorf("stack %s has no host yet; the placement chooses one", id)
	case *stack.Host == payload.To && stack.MoveTo != nil:
		// a move to where it runs takes the request back
		return func() {
			stack.MoveTo = nil
			s.changed(resource.KindStack, &stack.Meta)
		}, nil
	case *stack.Host == payload.To:
		return nil, fmt.Errorf("stack %s runs on host %d already", id, payload.To)
	case !ok || host.Phase != Active || host.Leaving || !host.Roles.Has(hostrole.Worker):
		return nil, fmt.Errorf("host %d is no active worker; a stack moves only to one", payload.To)
	case samePlace(stack.MoveTo, &payload.To):
		return nil, errUnchanged
	}
	to := payload.To
	return func() {
		stack.MoveTo = &to
		s.changed(resource.KindStack, &stack.Meta)
	}, nil
}

// checkLeaderStatus refuses a change of a stack's status by the leader if
// the leader planned it on an older stack (checkPlanned), if another
// operation's lock forbids it, or if it moves the stack. The leader places
// only a stack that has no host yet, because a move needs an operation
// that holds the stack's lock.
func (s *State) checkLeaderStatus(stack *Stack, payload SetStackStatus) error {
	if err := checkPlanned(resource.KindStack, payload.ID, &stack.Meta, payload.PlannedAt); err != nil {
		return err
	}
	lock := StackLock(payload.ID)
	if err := s.checkUnlocked(lock, payload.Operation); err != nil {
		return err
	}
	moves := payload.Host != nil && stack.Host != nil && *payload.Host != *stack.Host
	switch {
	case !moves:
	case FailsOver(s.Hosts[*stack.Host].Phase):
		// its host is off and stops nothing: the stack fails over at once
		if to := s.Hosts[*payload.Host]; to.Phase != Active {
			return fmt.Errorf("stack %s cannot fail over to host %s: it is %s", payload.ID, to.Name, to.Phase)
		}
	case payload.Operation == 0 || s.Locks[lock] != payload.Operation:
		return fmt.Errorf("stack %s runs on host %d; only an operation that holds its lock moves it",
			payload.ID, *stack.Host)
	}
	return nil
}

// FailsOver tells whether the stacks of a host in this phase start
// elsewhere without a move, because its server is off (fenced) or stays off
// (disabled). Until the leader has powered it off, a failed host may still
// run them.
func FailsOver(phase HostPhase) bool {
	return phase == Fenced || phase == Disabled
}

// checkHostReport refuses a host's report that does not match the stack as
// the state has it now. Only the host that runs the stack may report about
// it, and the report cannot move the stack to another host, because the
// leader may have moved it since the host read the state. The report must
// also be about the stack's current generation and restarts. A report about
// older ones was made before an apply or a restart whose containers have
// not come up yet.
func checkHostReport(stack *Stack, actor string, payload SetStackStatus) error {
	switch {
	case stack.Host == nil || actor != HostActor(*stack.Host) || !samePlace(stack.Host, payload.Host):
		return fmt.Errorf("stack %s: only its host reports about it, and only where it is", payload.ID)
	case payload.Generation != stack.Generation:
		return fmt.Errorf("stack %s: the report is about generation %d, but %d is applied",
			payload.ID, payload.Generation, stack.Generation)
	case !maps.EqualFunc(payload.Restarted, stack.Restarted, time.Time.Equal):
		return fmt.Errorf("stack %s: the report is about an older restart", payload.ID)
	}
	return nil
}

// SetPushedOut is one decision of the scheduler about pushed-out services.
// It holds the whole new list of each stack it touches (an empty list means
// none). It either pushes services out, possibly in several stacks, or
// brings one service back.
type SetPushedOut struct {
	Stacks map[string][]string `json:"stacks"` // by the stack's ID
	// PlannedAt is the index of the state the leader planned with
	// (checkPlanned).
	PlannedAt uint64 `json:"planned_at"`
}

func applySetPushedOut(s *State, _ Command, payload SetPushedOut) (func(), error) {
	// the new lists of the stacks whose list changes, by the stack's ID
	newLists := map[string][]string{}
	for _, id := range slices.Sorted(maps.Keys(payload.Stacks)) {
		stack, ok := s.Stacks[id]
		if !ok {
			return nil, fmt.Errorf("no stack %s", id)
		}
		if err := checkPlanned(resource.KindStack, id, &stack.Meta, payload.PlannedAt); err != nil {
			return nil, err
		}
		if err := s.checkUnlocked(StackLock(id), 0); err != nil {
			return nil, err
		}
		for _, service := range payload.Stacks[id] {
			if _, ok := stack.Services[service]; !ok {
				return nil, fmt.Errorf("stack %s has no service %s", id, service)
			}
		}
		pushedOut := slices.Compact(slices.Sorted(slices.Values(payload.Stacks[id])))
		if !slices.Equal(stack.PushedOut, pushedOut) {
			newLists[id] = pushedOut
		}
	}
	if len(newLists) == 0 {
		return nil, errUnchanged
	}
	return func() {
		for _, id := range slices.Sorted(maps.Keys(newLists)) {
			stack := s.Stacks[id]
			stack.PushedOut = newLists[id]
			s.changed(resource.KindStack, &stack.Meta)
		}
	}, nil
}

// SetStackWaitReason records why the placement cannot place a stack or a
// volume that it names; an empty reason clears it. It changes nothing else,
// so it never moves or stops a running stack. WaitingSince starts with the
// first reason, at the command's time.
type SetStackWaitReason struct {
	ID     string `json:"id"` // the stack's
	Reason string `json:"reason,omitempty"`
}

func applySetStackWaitReason(s *State, cmd Command, payload SetStackWaitReason) (func(), error) {
	stack, ok := s.Stacks[payload.ID]
	switch {
	case !ok:
		return nil, fmt.Errorf("no stack %s", payload.ID)
	case stack.WaitReason == payload.Reason:
		return nil, errUnchanged
	}
	if err := s.checkUnlocked(StackLock(payload.ID), 0); err != nil {
		return nil, err
	}
	return func() {
		switch {
		case payload.Reason == "":
			stack.WaitingSince = time.Time{}
		case stack.WaitReason == "":
			stack.WaitingSince = cmd.At
		}
		stack.WaitReason = payload.Reason
		s.changed(resource.KindStack, &stack.Meta)
	}, nil
}

func samePlace(a, b *HostID) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

// checkLiveHost accepts no host, or a host that is a member.
func (s *State) checkLiveHost(id *HostID) error {
	if id == nil {
		return nil
	}
	if host, ok := s.Hosts[*id]; !ok || host.Phase == Removed {
		return fmt.Errorf("host %d is not a member", *id)
	}
	return nil
}

// checkWorker accepts no host, or a member that is a worker: only a worker
// runs stacks and holds copies with a disk.
func (s *State) checkWorker(id *HostID) error {
	if err := s.checkLiveHost(id); err != nil || id == nil {
		return err
	}
	if host := s.Hosts[*id]; !host.Roles.Has(hostrole.Worker) {
		return fmt.Errorf("host %s has the roles %s, not %s", host.Name, host.Roles, hostrole.Worker)
	}
	return nil
}
