// Package scheduler decides where stacks and their volumes go and which
// services give way to others. It is pure logic: it reads the state and
// returns decisions, which the controller on the leader writes as commands.
//
// Every service reserves memory and CPUs (its own reservations, else the
// cluster's default); a host offers what it reported less system_reserve.
// A new stack goes to a host where all of it fits, else to one where its
// services push out services of lower effective priority, one service
// after the other (fitStack). A host that has the stack's data comes
// before one that must get a copy first; such a stack moves there (Move).
// The copies of the volumes are kept in copies.go.
package scheduler

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/webcodingcat/clowder/internal/clusterspec"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/resource"
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

// Move is a decision to move a stack to another host with a move-stack
// operation: a stack that runs elsewhere, or one without a host whose
// volumes are elsewhere.
type Move struct {
	Stack string
	// From is the stack's host; nil for a stack that has none yet.
	From *state.HostID
	To   state.HostID
	// PushedOut are the pushed-out lists on To, as for a placement.
	PushedOut map[string][]string
	// Why is what the log says: a user asked, the host drains, ...
	Why string
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
	Moves        []Move
	// Copies are the volumes' records with a copy added, a surplus copy
	// given up or the tiebreaker set (copies.go), one put-volume each.
	Copies  []state.Volume
	Waiting []Waiting
	// ComeBack is the pushed-out service that fits again first, if any; it
	// comes only when the plan places nothing and pushes nothing out, so a
	// waiting stack gets room before a pushed-out service does.
	ComeBack *ComeBack
}

// Plan makes hosts over capacity fit, places the volumes that placed stacks
// have gained and the stacks that have no host yet (highest priority
// first), moves the stacks off draining hosts and those a user asked to
// move, keeps the volumes' copies, and names the pushed-out service that
// may come back.
func Plan(current *state.State, now time.Time) Decisions {
	planner := newPlanner(current)
	planner.now = now
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
		switch {
		case current.Locks[state.StackLock(id)] != 0:
			continue // an operation holds it, and decides for it
		case stack.Host == nil:
			unplaced = append(unplaced, stack)
			continue
		}
		decisions.add(planner.keep(stack)...)
	}
	// by the highest effective priority of their services, from the
	// current categories like every other priority here
	slices.SortStableFunc(unplaced, func(a, b *state.Stack) int {
		return cmp.Compare(planner.stackPriority(b.ID()), planner.stackPriority(a.ID()))
	})
	for _, stack := range unplaced {
		decisions.add(planner.place(stack, planner.activeHosts(nil), "")...)
	}
	decisions.Copies = planner.keepCopies()
	// a pushed-out service comes back only in a pass that changes nothing
	// else: a waiting stack gets the room first
	if len(decisions.PushOuts) > 0 || len(decisions.Moves) > 0 ||
		slices.ContainsFunc(decisions.Placements, func(p Placement) bool { return p.SetsHost }) {
		return decisions
	}
	if ref, ok := planner.room.comingBack(); ok {
		pushedOut := slices.DeleteFunc(slices.Clone(current.Stacks[ref.stack].PushedOut),
			func(service string) bool { return service == ref.service })
		decisions.ComeBack = &ComeBack{Stack: ref.stack, Service: ref.service, PushedOut: pushedOut}
	}
	return decisions
}

// decision is what the planner decides for one stack: a placement, a
// move, or why it waits; any of them may be empty.
type decision struct {
	placement Placement
	move      *Move
	reason    string
}

// add takes the decisions for one stack.
func (d *Decisions) add(decided ...decision) {
	for _, one := range decided {
		d.addOne(one)
	}
}

func (d *Decisions) addOne(one decision) {
	switch {
	case one.reason != "":
		d.Waiting = append(d.Waiting, Waiting{Stack: one.placement.Stack, Reason: one.reason})
	case one.move != nil:
		d.Moves = append(d.Moves, *one.move)
	case one.placement.SetsHost || len(one.placement.Volumes) > 0:
		d.Placements = append(d.Placements, one.placement)
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
	// syncing is true while a copy with a disk is not synced yet, in the
	// state or by this plan: the resync limit (copies.go)
	syncing bool
	// tiebreakers counts the tiebreakers on each host, as planned, and
	// planned is each volume's last record in this plan, by ID
	// (countTiebreakers)
	tiebreakers map[state.HostID]int
	planned     map[string]state.Volume
	// now is the leader's clock, for the cooldown of moves (makeRoom)
	now time.Time
	// operation is the one the plan is for (MoveVolumes): what it locks is
	// the plan's to change
	operation uint64
}

func newPlanner(current *state.State) *planner {
	p := &planner{
		current: current, classes: current.Config.Spec.StorageClasses,
		allocated: map[state.HostID]map[string][]int64{}, minors: map[int]bool{}, room: newRoom(current),
		tiebreakers: map[state.HostID]int{}, planned: map[string]state.Volume{},
	}
	// a move that has not got its copies synced syncs them, all at once:
	// it counts as the one resync of the limit
	for _, operation := range current.Operations {
		if operation.Move != nil && operation.State == state.OpRunning && operation.Step < state.MoveSynced {
			p.syncing = true
		}
	}
	// a retired copy keeps its minor on its host until it is gone
	for minor := range current.RetiredMinors {
		p.minors[minor] = true
	}
	for _, volume := range current.Volumes {
		if volume.Minor != nil {
			p.minors[*volume.Minor] = true
		}
		for _, volumeCopy := range volume.Copies {
			switch {
			case volumeCopy.Tiebreaker:
				p.tiebreakers[volumeCopy.Host]++
			default:
				p.take(volumeCopy.Host, p.classes[volumeCopy.Class].Pool, int64(volume.Spec.Size))
				p.syncing = p.syncing || p.resyncs(volume, volumeCopy)
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

// takesStacks tells whether a host takes new stacks and copies: it is
// active, and no user removes it.
func (p *planner) takesStacks(id state.HostID) bool {
	host, ok := p.current.Hosts[id]
	return ok && host.Phase == state.Active && !host.Leaving
}

// activeHosts are the hosts that take stacks, sorted, but the one left out.
func (p *planner) activeHosts(leftOut *state.HostID) []state.HostID {
	var hosts []state.HostID
	for _, id := range slices.Sorted(maps.Keys(p.current.Hosts)) {
		if p.takesStacks(id) && (leftOut == nil || *leftOut != id) {
			hosts = append(hosts, id)
		}
	}
	return hosts
}

// keep decides for a stack that has a host: it fails it over if its host
// is down (fenced), moves it if its host drains or a user asked to (the
// volumes it newly names go with it), else it places those volumes where
// it runs.
func (p *planner) keep(stack *state.Stack) []decision {
	host := *stack.Host
	switch {
	case state.FailsOver(p.current.Hosts[host].Phase):
		return p.failover(stack)
	case p.current.Hosts[host].Phase == state.Draining || p.current.Hosts[host].Leaving:
		return p.place(stack, p.activeHosts(&host), "its host drains")
	case stack.MoveTo != nil:
		return p.place(stack, []state.HostID{*stack.MoveTo}, "a user asked to move it")
	}
	volumes, copies, reason := p.volumesOn(stack, host)
	switch {
	case reason == "" && len(copies) > 0:
		// the state lets a placed stack name only volumes with data where
		// it runs; a guard
		reason = "a volume has its data on another host"
	case reason == "" && len(volumes) > 0:
		p.reserve(host, volumes)
		return []decision{{placement: Placement{Stack: stack.ID(), Host: host, Volumes: volumes}}}
	case reason == "":
		return nil
	}
	return []decision{{placement: Placement{Stack: stack.ID()}, reason: reason}}
}

// place finds a host among hosts for a stack that has none, or for one
// that moves (why says why it moves), in this order:
//  1. a host that has the stack's data and where all of it fits, the one
//     with the least room left (best fit);
//  2. a host where all of it fits once its volumes get copies there, only
//     while no other copy syncs (the resync limit);
//  3. for a stack without a host, room made by moving stacks of lower
//     priority (makeRoom);
//  4. a host where its services push out others, with its data first.
//
// A reason means the stack waits.
func (p *planner) place(stack *state.Stack, hosts []state.HostID, why string) []decision {
	var whole, pushing []stackFit
	reasons := map[state.HostID]string{}
	for _, id := range hosts {
		if !p.takesStacks(id) {
			reasons[id] = "it takes no stacks"
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
	// a host without the stack's data comes after those with it: the copies
	// it needs must sync first, and a volume of replicas 0 starts there anew,
	// empty, as its data is thrown away when its stack moves (dataElsewhere)
	freeMemory := map[state.HostID]int64{}
	for _, fit := range whole {
		freeMemory[fit.host] = p.room.free(fit.host).memory
	}
	slices.SortStableFunc(whole, func(a, b stackFit) int { return cmp.Compare(freeMemory[a.host], freeMemory[b.host]) })
	if chosen := p.withDataFirst(stack, whole, reasons); len(chosen) > 0 {
		return []decision{p.choose(stack, chosen[0], why)}
	}
	if stack.Host == nil {
		if moves, reason, ok := p.makeRoom(stack); ok {
			return append(moves, decision{placement: Placement{Stack: stack.ID()}, reason: reason})
		}
	}
	if chosen := p.withDataFirst(stack, pushing, reasons); len(chosen) > 0 {
		return []decision{p.choose(stack, chosen[0], why)}
	}
	if len(reasons) == 0 {
		return []decision{{placement: Placement{Stack: stack.ID()}, reason: "no active host"}}
	}
	var each []string
	for _, id := range slices.Sorted(maps.Keys(reasons)) {
		each = append(each, p.current.Hosts[id].Name+": "+reasons[id])
	}
	reason := strings.Join(each, "; ")
	if why != "" {
		reason = "cannot move (" + why + "): " + reason
	}
	return []decision{{placement: Placement{Stack: stack.ID()}, reason: reason}}
}

// withDataFirst are the hosts of fits that can take the stack's volumes,
// those with its data first; reasons gets why the others cannot.
func (p *planner) withDataFirst(stack *state.Stack, fits []stackFit, reasons map[state.HostID]string) []candidate {
	var withData, withoutData []candidate
	for _, fit := range fits {
		volumes, copies, reason := p.volumesOn(stack, fit.host)
		switch {
		case reason != "":
			reasons[fit.host] = reason
		case len(copies) == 0 && !p.dataElsewhere(stack, fit.host):
			withData = append(withData, candidate{fit, volumes, nil})
		case p.syncing && len(copies) > 0:
			reasons[fit.host] = "a copy of a volume would have to sync there, and another copy syncs already"
		default:
			withoutData = append(withoutData, candidate{fit, volumes, copies})
		}
	}
	return slices.Concat(withData, withoutData)
}

// candidate is a host a stack fits, with what its volumes need there
// (volumesOn).
type candidate struct {
	fit             stackFit
	volumes, copies []state.Volume
}

// choose makes a candidate the stack's: a placement for a stack without a
// host whose data is there, else a move. A move the placement starts on
// its own waits while one of the stack failed recently (failedRecently);
// one a user asked for goes.
func (p *planner) choose(stack *state.Stack, chosen candidate, why string) decision {
	host := chosen.fit.host
	moves := stack.Host != nil || len(chosen.copies) > 0 || p.dataElsewhere(stack, host)
	if moves && stack.MoveTo == nil && p.failedRecently(stack) {
		return decision{placement: Placement{Stack: stack.ID()}, reason: fmt.Sprintf(
			"a move failed at %s; the next try comes after %s", stack.MoveFailed.Format(time.RFC3339), MoveCooldown)}
	}
	pushedOut := p.room.accept(stack.ID(), chosen.fit)
	p.reserve(host, slices.Concat(chosen.volumes, chosen.copies))
	if len(chosen.copies) > 0 {
		p.syncing = true
	}
	if !moves {
		return decision{placement: Placement{
			Stack: stack.ID(), Host: host, SetsHost: true, Volumes: chosen.volumes, PushedOut: pushedOut,
		}}
	}
	if why == "" {
		why = "its volumes are on another host"
	}
	return decision{
		placement: Placement{Stack: stack.ID()},
		move: &Move{
			Stack: stack.ID(), From: stack.Host, To: host,
			PushedOut: pushedOut, Why: why,
		},
	}
}

// hasData tells whether a host has a stack's data: none of its volumes
// needs a copy there, and none of replicas 0 has its LV elsewhere. A volume
// that waits for its first stack may go anywhere.
func (p *planner) hasData(stack *state.Stack, host state.HostID) bool {
	_, copies, reason := p.volumesOn(stack, host)
	return reason == "" && len(copies) == 0 && !p.dataElsewhere(stack, host)
}

// reserve takes the space and minors of volume records a decision places
// copies with: what is new in them beside the state's records.
func (p *planner) reserve(host state.HostID, volumes []state.Volume) {
	for _, volume := range volumes {
		index := slices.IndexFunc(volume.Copies, func(c state.VolumeCopy) bool { return c.Host == host })
		if index >= 0 && !volume.Copies[index].Tiebreaker {
			p.take(host, p.classes[volume.Copies[index].Class].Pool, int64(volume.Spec.Size))
		}
		if volume.Minor != nil {
			p.minors[*volume.Minor] = true
		}
		p.countTiebreakers(volume)
	}
}

// dataElsewhere tells whether a stack names a placed volume of replicas 0
// whose plain LV is on another host: such a stack moves (its move gives
// the volume a new, empty LV on the new host), but no copy syncs.
func (p *planner) dataElsewhere(stack *state.Stack, host state.HostID) bool {
	for _, name := range stack.Spec.VolumeNames() {
		volume := p.current.Volumes[names.ID(stack.Namespace, name)]
		if volume != nil && volume.Status != state.VolumePending && volume.Spec.Replicas == 0 &&
			!slices.ContainsFunc(volume.Copies, func(c state.VolumeCopy) bool { return c.Host == host }) {
			return true
		}
	}
	return false
}

// volumesOn works out what a stack's volumes need on host: volumes are
// those that wait for their first stack (pending), each placed there with
// its first copy; copies are placed volumes of replicas 1 and more that
// get a copy there (a tiebreaker there gets a disk), with the tiebreaker
// that needs (fitTiebreaker: a volume of replicas 1 keeps its quorum while
// the new copy syncs, should a host fail). A placed volume of replicas 0
// needs neither (dataElsewhere). Nothing is taken from the
// planner yet (reserve). A reason says why host cannot take them.
func (p *planner) volumesOn(stack *state.Stack, host state.HostID) (volumes, copies []state.Volume, reason string) {
	var reserved []allocation
	var minors []int
	for _, name := range stack.Spec.VolumeNames() {
		id := names.ID(stack.Namespace, name)
		current, ok := p.current.Volumes[id]
		switch {
		case !ok:
			return nil, nil, fmt.Sprintf("volume %s is gone", name) // the state refuses this; a guard
		case current.Status == state.VolumeRestorePending:
			return nil, nil, fmt.Sprintf("volume %s lost its data with the hosts clowderd recover retired; "+
				"it waits for a restore", name)
		case current.Status != state.VolumePending && current.Spec.Replicas == 0:
			continue
		case current.Status != state.VolumePending:
			index := slices.IndexFunc(current.Copies, func(c state.VolumeCopy) bool { return c.Host == host })
			if index >= 0 && !current.Copies[index].Tiebreaker {
				continue
			}
			if holder := p.current.Locks[state.VolumeLock(id)]; holder != 0 && holder != p.operation {
				return nil, nil, fmt.Sprintf("volume %s is locked by another operation", name)
			}
			if p.retiresHere(id, current.DataGeneration, host) {
				return nil, nil, fmt.Sprintf("volume %s: the host still removes an old copy", name)
			}
			class, pool, ok := p.fit(host, current.Spec.Class, int64(current.Spec.Size), reserved)
			if !ok {
				return nil, nil, noPool(name, current.Spec)
			}
			reserved = append(reserved, allocation{pool, int64(current.Spec.Size)})
			volume := *current
			volume.Copies = slices.DeleteFunc(slices.Clone(current.Copies), func(c state.VolumeCopy) bool { return c.Host == host })
			volume.Copies = append(volume.Copies, state.VolumeCopy{Host: host, Class: class})
			p.fitTiebreaker(&volume, nil)
			copies = append(copies, volume)
			continue
		}
		wanted := current.Spec
		class, pool, ok := p.fit(host, wanted.Class, int64(wanted.Size), reserved)
		if !ok {
			return nil, nil, noPool(name, wanted)
		}
		volume := *current
		volume.Copies = []state.VolumeCopy{{Host: host, Class: class}}
		volume.Status = state.VolumeOK
		if wanted.Replicas > 0 {
			minor, ok := p.freeMinor(minors)
			if !ok {
				return nil, nil, "no free DRBD minor"
			}
			minors = append(minors, minor)
			volume.Minor = &minor
		}
		reserved = append(reserved, allocation{pool, int64(wanted.Size)})
		volumes = append(volumes, volume)
	}
	return volumes, copies, ""
}

// noPool is the reason for a volume that fits no pool of a host.
func noPool(name string, spec resource.VolumeSpec) string {
	return fmt.Sprintf("volume %s (%s, class %s) fits no pool", name, units.FormatSize(int64(spec.Size)), spec.Class)
}

// retiresHere tells whether a host still removes a copy of the volume in
// its data generation: a new copy there would have the same LV.
func (p *planner) retiresHere(volumeID string, generation int64, host state.HostID) bool {
	return slices.Contains(p.current.Retired, state.RetiredCopy{Volume: volumeID, DataGeneration: generation, Host: host})
}

// allocation is space a volume takes in a pool.
type allocation struct {
	pool string
	size int64
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
