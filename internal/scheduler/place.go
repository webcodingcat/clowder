// Package scheduler decides where stacks and their volumes go and which
// services give way to others. It is pure logic: it reads the state and
// returns decisions, which the controller on the leader writes as commands.
//
// Every service reserves memory and CPUs (its own reservations, else the
// cluster's default); a host offers what it reported less system_reserve.
// A new stack goes to a host where all of it fits, else to one where its
// services push out services of lower effective priority, one service
// after the other (fitStack).
package scheduler

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/webcodingcat/clowder/internal/clusterspec"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/state"
	"github.com/webcodingcat/clowder/internal/units"
)

// Placement is a decision for one stack: the host and the volumes that are
// new there (all of them for a new stack, the added ones for an update).
type Placement struct {
	Stack string // the stack's ID, as every stack in the scheduler's results
	Host  state.HostID
	// SetsHost is true for a stack that gets Host as its host (set-stack-status
	// starting), false for one that keeps its host and only gets volumes.
	SetsHost bool
	Volumes  []state.Volume
	// PushedOut are the new pushed-out lists of the stacks a new stack
	// touches, its own included (set-pushed-out, before the placement).
	PushedOut map[string][]string
}

// Waiting is a stack that finds no place, with the reason.
type Waiting struct {
	Stack  string
	Reason string
}

// ComeBack brings one pushed-out service back.
type ComeBack struct {
	Stack, Service string
	// PushedOut is the stack's list without the service.
	PushedOut []string
}

// Decisions are what one plan decides, in the order the controller writes
// them.
type Decisions struct {
	// PushOuts make hosts over capacity fit again, one set-pushed-out each.
	PushOuts []map[string][]string
	// OverCapacity are the hosts that do not fit even so: their highest
	// priority alone reserves more than they offer.
	OverCapacity []state.HostID
	Placements   []Placement
	Waiting      []Waiting
	// ComeBack is the pushed-out service that fits again first, if any; it
	// comes only when the plan places nothing and pushes nothing out, so a
	// waiting stack gets room before a pushed-out service does.
	ComeBack *ComeBack
}

// Plan makes hosts over capacity fit, places the volumes that placed stacks
// have gained and the stacks that have no host yet (highest priority
// first), and names the pushed-out service that may come back.
func Plan(current *state.State) Decisions {
	planner := newPlanner(current)
	var decisions Decisions
	for _, id := range slices.Sorted(maps.Keys(current.Hosts)) {
		lists, over := planner.room.fitOverCapacity(id)
		if over {
			decisions.OverCapacity = append(decisions.OverCapacity, id)
		}
		if lists != nil {
			decisions.PushOuts = append(decisions.PushOuts, lists)
		}
	}
	var unplaced []*state.Stack
	for _, id := range slices.Sorted(maps.Keys(current.Stacks)) {
		stack := current.Stacks[id]
		if stack.Host == nil {
			unplaced = append(unplaced, stack)
			continue
		}
		decisions.add(planner.place(stack))
	}
	// by the highest effective priority of their services, from the
	// current categories like every other priority here
	highest := map[string]int{}
	for _, stack := range unplaced {
		for service := range stack.Services {
			highest[stack.ID()] = max(highest[stack.ID()], planner.room.priority(serviceRef{stack.ID(), service}))
		}
	}
	slices.SortStableFunc(unplaced, func(a, b *state.Stack) int { return cmp.Compare(highest[b.ID()], highest[a.ID()]) })
	for _, stack := range unplaced {
		decisions.add(planner.place(stack))
	}
	// a pushed-out service comes back only in a pass that changes nothing
	// else: a waiting stack gets the room first
	if len(decisions.PushOuts) > 0 || slices.ContainsFunc(decisions.Placements, func(p Placement) bool { return p.SetsHost }) {
		return decisions
	}
	if ref, ok := planner.room.comingBack(); ok {
		pushedOut := slices.DeleteFunc(slices.Clone(current.Stacks[ref.stack].PushedOut),
			func(service string) bool { return service == ref.service })
		decisions.ComeBack = &ComeBack{Stack: ref.stack, Service: ref.service, PushedOut: pushedOut}
	}
	return decisions
}

// add takes one stack's placement, or its reason to wait.
func (d *Decisions) add(placement Placement, reason string) {
	switch {
	case reason != "":
		d.Waiting = append(d.Waiting, Waiting{Stack: placement.Stack, Reason: reason})
	case placement.SetsHost || len(placement.Volumes) > 0:
		d.Placements = append(d.Placements, placement)
	}
}

// planner keeps what the decisions so far have taken, so two stacks in one
// plan do not both count on the same space, minor or room.
type planner struct {
	current *state.State
	classes map[string]clusterspec.StorageClass
	// allocated sums the volumes on each host's pools.
	allocated map[state.HostID]map[string][]int64
	minors    map[int]bool
	room      *room
}

func newPlanner(current *state.State) *planner {
	p := &planner{
		current: current, classes: current.Config.Spec.StorageClasses,
		allocated: map[state.HostID]map[string][]int64{}, minors: map[int]bool{}, room: newRoom(current),
	}
	for _, volume := range current.Volumes {
		if volume.Minor != nil {
			p.minors[*volume.Minor] = true
		}
		for _, volumeCopy := range volume.Copies {
			if !volumeCopy.Tiebreaker {
				p.take(volumeCopy.Host, p.classes[volumeCopy.Class].Pool, int64(volume.Spec.Size))
			}
		}
	}
	return p
}

func (p *planner) take(host state.HostID, pool string, size int64) {
	if p.allocated[host] == nil {
		p.allocated[host] = map[string][]int64{}
	}
	p.allocated[host][pool] = append(p.allocated[host][pool], size)
}

// place decides for one stack; a reason means it waits, and the placement
// then names only the stack. A new stack goes to the active host where all
// of it fits with the least room left (best fit), else to the first where
// its services push out others.
func (p *planner) place(stack *state.Stack) (Placement, string) {
	if stack.Host != nil {
		volumes, reason := p.newVolumes(stack, *stack.Host)
		return Placement{Stack: stack.ID(), Host: *stack.Host, Volumes: volumes}, reason
	}
	var whole, pushing []stackFit
	reasons := map[state.HostID]string{}
	for _, id := range slices.Sorted(maps.Keys(p.current.Hosts)) {
		if p.current.Hosts[id].Phase != state.Active {
			continue
		}
		fit, reason := p.room.fitStack(stack.ID(), id)
		switch {
		case reason != "":
			reasons[id] = reason
		case len(fit.pushed) == 0 && len(fit.own) == 0:
			whole = append(whole, fit)
		default:
			pushing = append(pushing, fit)
		}
	}
	freeMemory := map[state.HostID]int64{}
	for _, fit := range whole {
		freeMemory[fit.host] = p.room.free(fit.host).memory
	}
	slices.SortStableFunc(whole, func(a, b stackFit) int { return cmp.Compare(freeMemory[a.host], freeMemory[b.host]) })
	for _, fit := range append(whole, pushing...) {
		volumes, reason := p.newVolumes(stack, fit.host)
		if reason != "" {
			reasons[fit.host] = reason
			continue
		}
		return Placement{
			Stack: stack.ID(), Host: fit.host, SetsHost: true, Volumes: volumes,
			PushedOut: p.room.accept(stack.ID(), fit),
		}, ""
	}
	if len(reasons) == 0 {
		return Placement{Stack: stack.ID()}, "no active host"
	}
	var each []string
	for _, id := range slices.Sorted(maps.Keys(reasons)) {
		each = append(each, p.current.Hosts[id].Name+": "+reasons[id])
	}
	return Placement{Stack: stack.ID()}, strings.Join(each, "; ")
}

// allocation is space a volume takes in a pool.
type allocation struct {
	pool string
	size int64
}

// reservation is what one stack's new volumes would take on a host. It is
// kept apart from the planner until all of them fit: a stack that does not
// fit takes nothing, neither space nor minors.
type reservation struct {
	space  []allocation
	minors []int
}

// newVolumes places on host the volumes the stack names that wait for
// their first stack (pending), taking their space; a reason says why one
// does not fit, or why host is the wrong one: a placed volume stays where
// its copy is, so the stack must go there.
func (p *planner) newVolumes(stack *state.Stack, host state.HostID) ([]state.Volume, string) {
	var volumes []state.Volume
	var reserved reservation
	for _, name := range stack.Spec.VolumeNames() {
		current, ok := p.current.Volumes[names.ID(stack.Namespace, name)]
		switch {
		case !ok:
			return nil, fmt.Sprintf("volume %s is gone", name) // the state refuses this; a guard
		case current.Status != state.VolumePending:
			if !slices.ContainsFunc(current.Copies, func(volumeCopy state.VolumeCopy) bool {
				return volumeCopy.Host == host && !volumeCopy.Tiebreaker
			}) {
				return nil, fmt.Sprintf("volume %s has its data on another host", name)
			}
			continue
		}
		wanted := current.Spec
		class, pool, ok := p.fit(host, wanted.Class, int64(wanted.Size), reserved.space)
		if !ok {
			return nil, fmt.Sprintf("volume %s (%s, class %s) fits no pool", name,
				units.FormatSize(int64(wanted.Size)), wanted.Class)
		}
		volume := *current
		volume.Copies = []state.VolumeCopy{{Host: host, Class: class}}
		volume.Status = state.VolumeOK
		if wanted.Replicas > 0 {
			minor, ok := p.freeMinor(reserved.minors)
			if !ok {
				return nil, "no free DRBD minor"
			}
			reserved.minors = append(reserved.minors, minor)
			volume.Minor = &minor
		}
		reserved.space = append(reserved.space, allocation{pool, int64(wanted.Size)})
		volumes = append(volumes, volume)
	}
	for _, each := range reserved.space {
		p.take(host, each.pool, each.size)
	}
	for _, minor := range reserved.minors {
		p.minors[minor] = true
	}
	return volumes, ""
}

// fit finds a class whose pool on host holds size without overcommit,
// counting the space reserved beside what the planner has taken. A class
// with a pool of its own is the only candidate; a class without one stands
// for its fallbacks, tried in order.
func (p *planner) fit(host state.HostID, className string, size int64, reserved []allocation) (
	class, pool string, ok bool,
) {
	candidates := []string{className}
	if asked := p.classes[className]; asked.Pool == "" {
		candidates = asked.Fallback
	}
	for _, candidate := range candidates {
		spec := p.classes[candidate]
		reported, ok := p.current.Hosts[host].Pools[spec.Pool]
		if spec.Pool == "" || !ok {
			continue
		}
		allocated := slices.Clone(p.allocated[host][spec.Pool])
		for _, each := range reserved {
			if each.pool == spec.Pool {
				allocated = append(allocated, each.size)
			}
		}
		if Fits(reported.SizeBytes, spec.ReservePercent(), allocated, size) {
			return candidate, spec.Pool, true
		}
	}
	return "", "", false
}

// freeMinor finds the lowest DRBD minor that neither a volume has nor
// reserved holds.
func (p *planner) freeMinor(reserved []int) (int, bool) {
	for minor := range state.MaxMinor + 1 {
		if !p.minors[minor] && !slices.Contains(reserved, minor) {
			return minor, true
		}
	}
	return 0, false
}
