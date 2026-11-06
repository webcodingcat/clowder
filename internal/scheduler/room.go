package scheduler

import (
	"cmp"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"

	"github.com/webcodingcat/clowder/internal/clusterspec"
	"github.com/webcodingcat/clowder/internal/state"
	"github.com/webcodingcat/clowder/internal/units"
)

// resources are memory and CPUs as the scheduler counts them: memory in
// bytes, and CPUs in thousandths of a CPU, so that sums are exact.
type resources struct {
	memory    int64
	milliCPUs int64
}

func (r resources) plus(other resources) resources {
	return resources{r.memory + other.memory, r.milliCPUs + other.milliCPUs}
}

func (r resources) minus(other resources) resources {
	return resources{r.memory - other.memory, r.milliCPUs - other.milliCPUs}
}

func (r resources) within(room resources) bool {
	return r.memory <= room.memory && r.milliCPUs <= room.milliCPUs
}

// short tells whether free room is below zero: more is reserved than there
// is.
func (r resources) short() bool {
	return r.memory < 0 || r.milliCPUs < 0
}

// comeBackMarginPercent is the spare room that a pushed-out service needs
// to come back, so that it is not pushed out again at the next small
// change.
const comeBackMarginPercent = 5

func (r resources) withMargin() resources {
	scale := func(value int64) int64 { return value * (100 + comeBackMarginPercent) / 100 }
	return resources{scale(r.memory), scale(r.milliCPUs)}
}

func (r resources) String() string {
	return units.FormatSize(r.memory) + " and " + strconv.FormatFloat(float64(r.milliCPUs)/1000, 'f', -1, 64) + " CPUs"
}

// parseResources ignores errors, because WithDefaults filled the resources
// in and Check made sure that they parse.
func parseResources(config clusterspec.Resources) resources {
	memory, _ := units.ParseSize(config.Memory)
	return resources{memory, milli(config.CPUs)}
}

func milli(cpus float64) int64 {
	return int64(math.Round(cpus * 1000))
}

// serviceRef names one service of one stack, by the stack's ID.
type serviceRef struct {
	stack, service string
}

// room tracks what the services of the stacks on each host take, as the
// decisions of a plan change it. A pushed-out service takes nothing.
type room struct {
	// current is the state that the plan starts from; the plan's own
	// decisions are in pushedOut and placed
	current   *state.State
	defaults  resources // the reservation of a service without its own
	reserve   resources // what each host keeps for itself
	pushedOut map[string]map[string]bool
	placed    map[string]state.HostID
	effective map[string]map[string]int // a cache
}

// HostRoom is what a host offers its services and what they reserve, as
// the scheduler counts it: memory in bytes, and CPUs in thousandths.
type HostRoom struct {
	OfferedMemory, ReservedMemory       int64
	OfferedMilliCPUs, ReservedMilliCPUs int64
}

// Rooms returns the room of each host that is not removed, as the
// scheduler counts it when it places, for /metrics.
func Rooms(current *state.State) map[state.HostID]HostRoom {
	counted := newRoom(current)
	rooms := map[state.HostID]HostRoom{}
	for id, host := range current.Hosts {
		if host.Phase == state.Removed {
			continue
		}
		offered, free := counted.capacity(id), counted.free(id)
		reserved := offered.minus(free)
		rooms[id] = HostRoom{
			OfferedMemory: offered.memory, ReservedMemory: reserved.memory,
			OfferedMilliCPUs: offered.milliCPUs, ReservedMilliCPUs: reserved.milliCPUs,
		}
	}
	return rooms
}

func newRoom(current *state.State) *room {
	config := current.Config.Spec.WithDefaults()
	r := &room{
		current: current, defaults: parseResources(config.Defaults.Reservations),
		reserve: parseResources(config.SystemReserve), pushedOut: map[string]map[string]bool{},
		placed: map[string]state.HostID{}, effective: map[string]map[string]int{},
	}
	for id, stack := range current.Stacks {
		r.pushedOut[id] = map[string]bool{}
		for _, service := range stack.PushedOut {
			r.pushedOut[id][service] = true
		}
	}
	return r
}

// hostOf returns the host where a stack runs or is planned to run. A
// failing stack keeps its place, because its host tries again at every
// pass. A stack that moves counts on the host that it moves to, so that
// the room there is taken from the start.
func (r *room) hostOf(id string) (state.HostID, bool) {
	if host, ok := r.placed[id]; ok {
		return host, true
	}
	if move := r.current.MoveOf(id); move != nil && move.State == state.OpRunning {
		return move.Move.To, true
	}
	stack := r.current.Stacks[id]
	if stack.Host == nil || stack.Status == state.StackPending {
		return 0, false
	}
	return *stack.Host, true
}

// stillOn returns the old host of a stack that moves and has not stopped
// there yet.
func (r *room) stillOn(id string) (state.HostID, bool) {
	move := r.current.MoveOf(id)
	if move == nil || move.State != state.OpRunning || move.Move.From == nil || move.Step >= state.MoveStopped {
		return 0, false
	}
	return *move.Move.From, true
}

func (r *room) capacity(host state.HostID) resources {
	reported := r.current.Hosts[host].Capacity
	return resources{reported.MemoryBytes, milli(reported.CPUs)}.minus(r.reserve)
}

// reservation returns what a service takes. A reservation left out is
// stored as 0, so one written as 0 also gets the default, and every
// service reserves something.
func (r *room) reservation(ref serviceRef) resources {
	service := r.current.Stacks[ref.stack].Services[ref.service]
	taken := resources{service.MemoryBytes, milli(service.CPUs)}
	if taken.memory == 0 {
		taken.memory = r.defaults.memory
	}
	if taken.milliCPUs == 0 {
		taken.milliCPUs = r.defaults.milliCPUs
	}
	return taken
}

// running returns the services that take room on a host, sorted. A stack
// that moves takes room on both hosts until it has stopped on the old one;
// otherwise a stack placed in the room that it leaves would run beside it
// for a while.
func (r *room) running(host state.HostID) []serviceRef {
	var refs []serviceRef
	for _, id := range slices.Sorted(maps.Keys(r.current.Stacks)) {
		on, ok := r.hostOf(id)
		old, moving := r.stillOn(id)
		if (!ok || on != host) && (!moving || old != host) {
			continue
		}
		for _, service := range slices.Sorted(maps.Keys(r.current.Stacks[id].Services)) {
			if !r.pushedOut[id][service] {
				refs = append(refs, serviceRef{id, service})
			}
		}
	}
	return refs
}

// free is negative when the host is over capacity.
func (r *room) free(host state.HostID) resources {
	free := r.capacity(host)
	for _, ref := range r.running(host) {
		free = free.minus(r.reservation(ref))
	}
	return free
}

// priority returns a service's effective priority: the lowest of its own
// priority and those of everything it depends on, so that a running
// service never depends on a stopped one.
func (r *room) priority(ref serviceRef) int {
	if r.effective[ref.stack] == nil {
		r.effective[ref.stack] = r.effectiveOf(ref.stack)
	}
	return r.effective[ref.stack][ref.service]
}

// effectiveOf resolves the categories with the current configuration, so
// that a changed category takes effect without a new apply.
func (r *room) effectiveOf(id string) map[string]int {
	stack := r.current.Stacks[id]
	priorities := r.current.Config.Spec.WithDefaults().Priorities
	effective := map[string]int{}
	var resolve func(service string, seen map[string]bool) int
	resolve = func(service string, seen map[string]bool) int {
		if value, ok := effective[service]; ok {
			return value
		}
		own, _ := priorities.Resolve(stack.Services[service].Priority) // never fails: the state checked it
		seen[service] = true
		for _, dependency := range stack.Services[service].DependsOn {
			if !seen[dependency] { // a guard: stackspec.Check refuses cycles
				own = min(own, resolve(dependency, seen))
			}
		}
		delete(seen, service)
		effective[service] = own
		return own
	}
	for service := range stack.Services {
		resolve(service, map[string]bool{})
	}
	return effective
}

// depth returns the length of the longest chain of services that a service
// depends on.
func depth(stack *state.Stack, service string, seen map[string]bool) int {
	deepest := 0
	seen[service] = true
	for _, dependency := range stack.Services[service].DependsOn {
		if !seen[dependency] {
			deepest = max(deepest, 1+depth(stack, dependency, seen))
		}
	}
	delete(seen, service)
	return deepest
}

// startOrder sorts services by effective priority, highest first, and at
// equal priority the services that others depend on first.
func (r *room) startOrder(refs []serviceRef) {
	slices.SortFunc(refs, func(a, b serviceRef) int {
		return cmp.Or(
			cmp.Compare(r.priority(b), r.priority(a)),
			cmp.Compare(depth(r.current.Stacks[a.stack], a.service, map[string]bool{}),
				depth(r.current.Stacks[b.stack], b.service, map[string]bool{})),
			cmp.Compare(a.stack, b.stack), cmp.Compare(a.service, b.service))
	})
}

// withDependents returns a service and every service of its stack that
// depends on it, also indirectly.
func (r *room) withDependents(ref serviceRef) []serviceRef {
	stack := r.current.Stacks[ref.stack]
	found := map[string]bool{ref.service: true}
	for grew := true; grew; {
		grew = false
		for _, service := range slices.Sorted(maps.Keys(stack.Services)) {
			if found[service] {
				continue
			}
			for _, dependency := range stack.Services[service].DependsOn {
				if found[dependency] {
					found[service], grew = true, true
					break
				}
			}
		}
	}
	var refs []serviceRef
	for _, service := range slices.Sorted(maps.Keys(found)) {
		refs = append(refs, serviceRef{ref.stack, service})
	}
	return refs
}

// victims chooses running services of a host below a priority to push
// out, lowest first, each with what depends on it, until need fits into
// free. Services in gone already count as pushed out.
func (r *room) victims(host state.HostID, below int, need, free resources, gone map[serviceRef]bool) (
	victims []serviceRef, freeAfter resources, ok bool,
) {
	var candidates []serviceRef
	for _, ref := range r.running(host) {
		if !gone[ref] && r.priority(ref) < below {
			candidates = append(candidates, ref)
		}
	}
	slices.SortFunc(candidates, func(a, b serviceRef) int {
		return cmp.Or(cmp.Compare(r.priority(a), r.priority(b)),
			cmp.Compare(a.stack, b.stack), cmp.Compare(a.service, b.service))
	})
	chosen := map[serviceRef]bool{}
	for _, candidate := range candidates {
		if need.within(free) {
			break
		}
		for _, ref := range r.withDependents(candidate) {
			if !chosen[ref] && !gone[ref] && !r.pushedOut[ref.stack][ref.service] {
				chosen[ref] = true
				victims = append(victims, ref)
				free = free.plus(r.reservation(ref))
			}
		}
	}
	return victims, free, need.within(free)
}

// pushOut writes the new lists of the services' stacks into lists, in the
// form that set-pushed-out takes.
func (r *room) pushOut(refs []serviceRef, lists map[string][]string) {
	for _, ref := range refs {
		r.pushedOut[ref.stack][ref.service] = true
	}
	for _, ref := range refs {
		lists[ref.stack] = slices.Sorted(maps.Keys(r.pushedOut[ref.stack]))
	}
}

// fitOverCapacity pushes out services of a host whose reservations exceed
// its capacity (placement.md, "Over capacity"). If that is not enough, it
// pushes out nothing, and the host stays over capacity.
func (r *room) fitOverCapacity(host state.HostID) (lists map[string][]string, over bool) {
	free := r.free(host)
	running := r.running(host)
	if len(running) == 0 || !free.short() {
		return nil, false
	}
	highest := 0
	for _, ref := range running {
		highest = max(highest, r.priority(ref))
	}
	// victims pushes out until need fits into the free room; with an
	// empty need, that means until the free room is no longer below zero
	nothing := resources{}
	victims, _, ok := r.victims(host, highest, nothing, free, nil)
	if !ok {
		return nil, true
	}
	lists = map[string][]string{}
	r.pushOut(victims, lists)
	return lists, false
}

type stackFit struct {
	host state.HostID
	// pushed are the services of other stacks that the stack pushes out;
	// own are the stack's own services that start pushed out
	pushed, own []serviceRef
}

// fitStack plans a new stack's services on a host (placement.md, "Pushing
// out"). If the stack's first service finds no room, the stack does not
// fit at all. Nothing changes until accept.
func (r *room) fitStack(id string, host state.HostID) (stackFit, string) {
	stack := r.current.Stacks[id]
	var order []serviceRef
	for service := range stack.Services {
		order = append(order, serviceRef{id, service})
	}
	r.startOrder(order)
	free := r.free(host)
	gone := map[serviceRef]bool{} // what earlier services of the stack push out
	ownOut := map[string]bool{}
	fit := stackFit{host: host}
	for index, ref := range order {
		if ownOut[ref.service] {
			continue // depends on a service that found no room
		}
		need := r.reservation(ref)
		victims, freeAfter, ok := r.victims(host, r.priority(ref), need, free, gone)
		if !ok {
			if index == 0 {
				return stackFit{}, fmt.Sprintf("service %s (%s) finds no room, also not by pushing out lower priorities",
					ref.service, need)
			}
			for _, out := range r.withDependents(ref) {
				ownOut[out.service] = true
			}
			continue
		}
		for _, victim := range victims {
			gone[victim] = true
		}
		fit.pushed = append(fit.pushed, victims...)
		free = freeAfter.minus(need)
	}
	for _, service := range slices.Sorted(maps.Keys(ownOut)) {
		fit.own = append(fit.own, serviceRef{id, service})
	}
	return fit, ""
}

// accept makes a planned fit part of the plan and returns the pushed-out
// lists to write. The new stack's list is always among them, so that a
// list left by an earlier leader is replaced.
func (r *room) accept(id string, fit stackFit) map[string][]string {
	r.placed[id] = fit.host
	clear(r.pushedOut[id])
	lists := map[string][]string{id: {}}
	r.pushOut(append(slices.Clone(fit.pushed), fit.own...), lists)
	return lists
}

// comingBack returns the pushed-out service that may come back first: of
// those whose dependencies all run, in start order, the first whose
// reservation plus a margin fits.
func (r *room) comingBack() (serviceRef, bool) {
	var candidates []serviceRef
	for _, id := range slices.Sorted(maps.Keys(r.current.Stacks)) {
		if _, ok := r.hostOf(id); !ok {
			continue
		}
		stack := r.current.Stacks[id]
		for _, service := range slices.Sorted(maps.Keys(r.pushedOut[id])) {
			ready := true
			for _, dependency := range stack.Services[service].DependsOn {
				ready = ready && !r.pushedOut[id][dependency]
			}
			if ready {
				candidates = append(candidates, serviceRef{id, service})
			}
		}
	}
	r.startOrder(candidates)
	for _, ref := range candidates {
		host, _ := r.hostOf(ref.stack)
		if r.reservation(ref).withMargin().within(r.free(host)) {
			return ref, true
		}
	}
	return serviceRef{}, false
}

// roomSave lets a plan go back when an attempt does not work out
// (makeRoom).
type roomSave struct {
	pushedOut map[string]map[string]bool
	placed    map[string]state.HostID
}

func (r *room) save() roomSave {
	saved := roomSave{pushedOut: map[string]map[string]bool{}, placed: maps.Clone(r.placed)}
	for id, services := range r.pushedOut {
		saved.pushedOut[id] = maps.Clone(services)
	}
	return saved
}

func (r *room) restore(saved roomSave) {
	r.pushedOut, r.placed = saved.pushedOut, saved.placed
}
