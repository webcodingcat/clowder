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

// resources are memory and CPUs as the scheduler counts them: bytes and
// thousandths of a CPU, so sums are exact.
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

// within tells whether r fits into room.
func (r resources) within(room resources) bool {
	return r.memory <= room.memory && r.milliCPUs <= room.milliCPUs
}

// short tells whether free room is below zero: more is reserved than there
// is.
func (r resources) short() bool {
	return r.memory < 0 || r.milliCPUs < 0
}

// withMargin is r and 5 % more: a pushed-out service comes back only with
// room to spare, so it does not go again at the next small change.
func (r resources) withMargin() resources {
	return resources{r.memory * 105 / 100, r.milliCPUs * 105 / 100}
}

func (r resources) String() string {
	return units.FormatSize(r.memory) + " and " + strconv.FormatFloat(float64(r.milliCPUs)/1000, 'f', -1, 64) + " CPUs"
}

// readResources reads resources of cluster.yaml; WithDefaults filled them
// in and Check made sure they read.
func readResources(config clusterspec.Resources) resources {
	memory, _ := units.ParseSize(config.Memory)
	cpus, _ := strconv.ParseFloat(config.CPUs, 64)
	return resources{memory, milli(cpus)}
}

func milli(cpus float64) int64 {
	return int64(math.Round(cpus * 1000))
}

// serviceRef names one service of one stack, by the stack's ID.
type serviceRef struct {
	stack, service string
}

func (s serviceRef) String() string {
	return s.stack + "/" + s.service
}

// room is what the services of the stacks on the hosts take, as the
// decisions of a plan change it: a pushed-out service takes nothing.
type room struct {
	// current is the state the plan starts from; the plan's own decisions
	// are in pushedOut and placed
	current  *state.State
	defaults resources // a service's reservation without its own
	reserve  resources // what each host keeps for itself
	// pushedOut are the pushed-out services of each stack, as planned
	pushedOut map[string]map[string]bool
	// placed are the stacks this plan puts on a host
	placed map[string]state.HostID
	// effective caches each stack's effective priorities
	effective map[string]map[string]int
}

// HostRoom is what a host offers its services and what they reserve, as
// the scheduler counts it: memory in bytes, CPUs in thousandths.
type HostRoom struct {
	OfferedMemory, ReservedMemory       int64
	OfferedMilliCPUs, ReservedMilliCPUs int64
}

// Rooms is the room of each host that is not removed, as the scheduler
// counts it for placing (/metrics shows it).
func Rooms(current *state.State) map[state.HostID]HostRoom {
	r := newRoom(current)
	rooms := map[state.HostID]HostRoom{}
	for id, host := range current.Hosts {
		if host.Phase == state.Removed {
			continue
		}
		offered, free := r.capacity(id), r.free(id)
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
		current: current, defaults: readResources(config.Defaults.Reservations),
		reserve: readResources(config.SystemReserve), pushedOut: map[string]map[string]bool{},
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

// hostOf is where a stack runs or is planned to: false for a stack that
// has no host or does not run there yet or any more.
func (r *room) hostOf(id string) (state.HostID, bool) {
	if host, ok := r.placed[id]; ok {
		return host, true
	}
	stack := r.current.Stacks[id]
	if stack.Host == nil || (stack.Status != state.StackStarting && stack.Status != state.StackRunning) {
		return 0, false
	}
	return *stack.Host, true
}

// capacity is what a host offers its services: what it reported less
// system_reserve.
func (r *room) capacity(host state.HostID) resources {
	reported := r.current.Hosts[host].Capacity
	return resources{reported.MemoryBytes, milli(reported.CPUs)}.minus(r.reserve)
}

// reservation is what a service takes: its own reservations, else the
// cluster's default. State.Services keeps a reservation left out as 0, so
// one written as 0 takes the default too: no service reserves nothing.
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

// running are the services that take room on a host: those of the stacks
// there that are not pushed out, sorted.
func (r *room) running(host state.HostID) []serviceRef {
	var refs []serviceRef
	for _, id := range slices.Sorted(maps.Keys(r.current.Stacks)) {
		if on, ok := r.hostOf(id); !ok || on != host {
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

// free is a host's capacity less what its running services reserve;
// negative when the host is over capacity.
func (r *room) free(host state.HostID) resources {
	free := r.capacity(host)
	for _, ref := range r.running(host) {
		free = free.minus(r.reservation(ref))
	}
	return free
}

// priority is a service's effective priority: the lowest of its own and
// those of everything it depends on, since stopping a service stops what
// depends on it. This way a service is never pushed out before what
// depends on it, and a running service never depends on a stopped one.
func (r *room) priority(ref serviceRef) int {
	if r.effective[ref.stack] == nil {
		r.effective[ref.stack] = r.effectiveOf(ref.stack)
	}
	return r.effective[ref.stack][ref.service]
}

// effectiveOf works out the effective priorities of a stack's services.
// The categories come from the current cluster.yaml, so a changed category
// counts without a new apply; the state keeps every category a stack names.
func (r *room) effectiveOf(id string) map[string]int {
	stack := r.current.Stacks[id]
	priorities := r.current.Config.Spec.WithDefaults().Priorities
	effective := map[string]int{}
	var resolve func(service string, seen map[string]bool) int
	resolve = func(service string, seen map[string]bool) int {
		if value, ok := effective[service]; ok {
			return value
		}
		own, _ := priorities.Resolve(stack.Services[service].Priority) // resolves: the state checked it
		seen[service] = true
		for _, dependency := range stack.Services[service].DependsOn {
			if !seen[dependency] { // Compose refuses cycles; this only guards
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

// depth is how many services a service depends on in a row: at equal
// priority, a service comes after everything it depends on.
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

// startOrder sorts services highest effective priority first; at equal
// priority what others depend on first, then by stack and name.
func (r *room) startOrder(refs []serviceRef) {
	slices.SortFunc(refs, func(a, b serviceRef) int {
		return cmp.Or(
			cmp.Compare(r.priority(b), r.priority(a)),
			cmp.Compare(depth(r.current.Stacks[a.stack], a.service, map[string]bool{}),
				depth(r.current.Stacks[b.stack], b.service, map[string]bool{})),
			cmp.Compare(a.stack, b.stack), cmp.Compare(a.service, b.service))
	})
}

// withDependents is a service and every service of its stack that depends
// on it, directly or not.
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
// free; services in gone already count as pushed out. ok is false if even
// all of them would not make room; free is the room with them gone.
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

// pushOut marks services as pushed out and returns the new lists of the
// stacks they belong to, as set-pushed-out takes them.
func (r *room) pushOut(refs []serviceRef, lists map[string][]string) {
	for _, ref := range refs {
		r.pushedOut[ref.stack][ref.service] = true
	}
	for _, ref := range refs {
		lists[ref.stack] = slices.Sorted(maps.Keys(r.pushedOut[ref.stack]))
	}
}

// fitOverCapacity pushes out services of a host whose reservations exceed
// its capacity (system_reserve was raised, a stack reserves more): below
// the highest effective priority there, lowest first, until they fit.
// Services at the highest priority never go for this; if pushing out all
// below it is not enough, nothing goes and the host stays over capacity.
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
	// push out until nothing more is needed: the free room is back at zero
	nothing := resources{}
	victims, _, ok := r.victims(host, highest, nothing, free, nil)
	if !ok {
		return nil, true
	}
	lists = map[string][]string{}
	r.pushOut(victims, lists)
	return lists, false
}

// stackFit is how a new stack fits on a host.
type stackFit struct {
	host state.HostID
	// pushed are the services of other stacks it pushes out; own those of
	// the new stack that start pushed out
	pushed, own []serviceRef
}

// fitStack plans a new stack's services on a host, highest effective
// priority first. Each takes free room or pushes out running services below
// its own priority; one that finds no room even so starts pushed out, with
// what depends on it. A reason says the stack does not fit at all: its
// first service finds no room. Nothing changes until accept.
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

// accept makes a planned fit part of the plan: the stack is on its host,
// the services it pushes out are out, and so are its own that found no
// room. It returns the pushed-out lists to write; the new stack's is always
// among them, so a list left from an earlier leader is replaced.
func (r *room) accept(id string, fit stackFit) map[string][]string {
	r.placed[id] = fit.host
	clear(r.pushedOut[id])
	lists := map[string][]string{id: {}}
	r.pushOut(append(slices.Clone(fit.pushed), fit.own...), lists)
	return lists
}

// comingBack is the pushed-out service that may come back first: of the
// services whose dependencies all run, highest effective priority first,
// the first whose reservation with a margin fits its host's free room.
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
