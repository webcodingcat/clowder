// Package scheduler decides where stacks and their volumes go. It is pure
// logic: it reads the state and returns decisions, which the controller on
// the leader writes as commands.
//
// So far (phase 1a, one host): a stack goes to the first active host where
// every volume fits its pool without overcommit, its storage class's
// fallbacks included. Reservations, priorities and pushing out come in 1b.
package scheduler

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/webcodingcat/clowder/internal/clusterdir"
	"github.com/webcodingcat/clowder/internal/compose"
	"github.com/webcodingcat/clowder/internal/state"
)

// maxMinor is the highest DRBD minor (state checks the same bound).
const maxMinor = 999

// Placement is a decision for one stack: the host and the volumes that are
// new there (all of them for a new stack, the added ones for an update).
type Placement struct {
	Stack string
	Host  state.HostID
	// Place is false for a stack that keeps its host and only gets volumes.
	Place   bool
	Volumes []state.Volume
}

// Waiting is a stack that finds no place, with the reason.
type Waiting struct {
	Stack  string
	Reason string
}

// Plan places the stacks that have no host yet and the volumes that placed
// stacks have gained.
func Plan(ctx context.Context, current *state.State) ([]Placement, []Waiting) {
	planner := newPlanner(current)
	var placements []Placement
	var waiting []Waiting
	for _, name := range slices.Sorted(maps.Keys(current.Stacks)) {
		stack := current.Stacks[name]
		parsed, err := compose.Parse(ctx, name, []byte(stack.Source))
		if err != nil {
			waiting = append(waiting, Waiting{Stack: name, Reason: err.Error()})
			continue
		}
		placement, reason := planner.place(stack, parsed)
		switch {
		case reason != "":
			waiting = append(waiting, Waiting{Stack: name, Reason: reason})
		case placement.Place || len(placement.Volumes) > 0:
			placements = append(placements, placement)
		}
	}
	return placements, waiting
}

// planner keeps what the decisions so far have taken, so two stacks in one
// plan do not both count on the same space or minor.
type planner struct {
	current *state.State
	classes map[string]clusterdir.StorageClass
	// allocated sums the volumes on each host's pools.
	allocated map[state.HostID]map[string][]int64
	minors    map[int]bool
}

func newPlanner(current *state.State) *planner {
	p := &planner{
		current: current, classes: current.Config.Spec.StorageClasses,
		allocated: map[state.HostID]map[string][]int64{}, minors: map[int]bool{},
	}
	for _, volume := range current.Volumes {
		if volume.Minor != nil {
			p.minors[*volume.Minor] = true
		}
		for _, placement := range volume.Placements {
			if !placement.Tiebreaker {
				p.take(placement.Host, p.classes[placement.Class].Pool, volume.SizeBytes)
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

// place decides for one stack; a reason means it waits.
func (p *planner) place(stack *state.Stack, parsed *compose.Stack) (Placement, string) {
	if stack.Host != nil {
		volumes, reason := p.newVolumes(stack.Name, *stack.Host, parsed)
		return Placement{Stack: stack.Name, Host: *stack.Host, Volumes: volumes}, reason
	}
	var reasons []string
	for _, id := range slices.Sorted(maps.Keys(p.current.Hosts)) {
		if p.current.Hosts[id].Phase != state.Active {
			continue
		}
		volumes, reason := p.newVolumes(stack.Name, id, parsed)
		if reason == "" {
			return Placement{Stack: stack.Name, Host: id, Place: true, Volumes: volumes}, ""
		}
		reasons = append(reasons, p.current.Hosts[id].Name+": "+reason)
	}
	if len(reasons) == 0 {
		return Placement{}, "no active host"
	}
	return Placement{}, fmt.Sprint(reasons)
}

// allocation is space a volume takes in a pool.
type allocation struct {
	pool string
	size int64
}

// newVolumes places the stack's volumes that are not in the state yet on
// host, taking their space; a reason says why one does not fit.
func (p *planner) newVolumes(stackName string, host state.HostID, parsed *compose.Stack) ([]state.Volume, string) {
	var volumes []state.Volume
	var tentative []allocation
	// a stack that does not fit takes nothing: neither space nor minors
	var minors []int
	giveBack := func() {
		for _, minor := range minors {
			delete(p.minors, minor)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(parsed.Volumes)) {
		id := stackName + "/" + name
		if _, exists := p.current.Volumes[id]; exists {
			continue
		}
		wanted := parsed.Volumes[name]
		class, pool, ok := p.fit(host, wanted.Class, wanted.Size, tentative)
		if !ok {
			giveBack()
			return nil, fmt.Sprintf("volume %s (%d bytes, class %s) fits no pool", name, wanted.Size, wanted.Class)
		}
		volume := state.Volume{
			ID: id, Generation: 1, Class: wanted.Class, SizeBytes: wanted.Size, Replicas: wanted.Replicas,
			Placements: []state.Placement{{Host: host, Class: class}}, Status: state.VolumeOK,
		}
		if wanted.Replicas > 0 {
			minor, ok := p.freeMinor()
			if !ok {
				giveBack()
				return nil, "no free DRBD minor"
			}
			minors = append(minors, minor)
			volume.Minor = &minor
		}
		tentative = append(tentative, allocation{pool, wanted.Size})
		volumes = append(volumes, volume)
	}
	for _, each := range tentative {
		p.take(host, each.pool, each.size)
	}
	return volumes, ""
}

// fit finds the class (the one asked for, or one of its fallbacks) whose
// pool on host holds size without overcommit.
func (p *planner) fit(host state.HostID, className string, size int64, tentative []allocation) (string, string, bool) {
	candidates := []string{className}
	if class := p.classes[className]; class.Pool == "" {
		candidates = class.Fallback
	}
	for _, candidate := range candidates {
		class := p.classes[candidate]
		pool, reported := p.current.Hosts[host].Pools[class.Pool]
		if class.Pool == "" || !reported {
			continue
		}
		allocated := slices.Clone(p.allocated[host][class.Pool])
		for _, each := range tentative {
			if each.pool == class.Pool {
				allocated = append(allocated, each.size)
			}
		}
		if Fits(pool.SizeBytes, class.ReservePercent(), allocated, size) {
			return candidate, class.Pool, true
		}
	}
	return "", "", false
}

// freeMinor takes the lowest DRBD minor no volume has.
func (p *planner) freeMinor() (int, bool) {
	for minor := range maxMinor + 1 {
		if !p.minors[minor] {
			p.minors[minor] = true
			return minor, true
		}
	}
	return 0, false
}
