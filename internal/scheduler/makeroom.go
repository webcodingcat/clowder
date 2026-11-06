package scheduler

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/webcodingcat/clowder/internal/state"
)

// MoveCooldown is how long a stack that an operation moved stays where it
// is before the placement moves it again to make room for another stack.
// This keeps a stack from being pushed from host to host.
const MoveCooldown = 10 * time.Minute

// makeRoom makes room for a stack without a host. It moves stacks of lower
// priority off a host that has the stack's data, to hosts where all of
// each moved stack fits and that have its data (no resync). It is the third
// way to place a stack: after the free room, and before pushing services
// out (place). A stack moved within MoveCooldown, or one that an operation
// holds, stays.
//
// The moves are the decisions, and the stack waits (the reason). It finds
// the room once the moved stacks have stopped on the host, because
// room.running counts a moving stack on both hosts until then. So while
// stacks move off a host, the stack waits for them instead of sending more
// stacks away.
func (p *planner) makeRoom(stack *state.Stack) (moves []decision, reason string, ok bool) {
	priority := p.stackPriority(stack.ID())
	for _, host := range p.activeHosts(nil) {
		if !p.hasData(stack, host) {
			continue
		}
		if leaving := p.movingOff(host); len(leaving) > 0 {
			return nil, "waits for " + strings.Join(leaving, ", ") + " to move off " + p.current.Hosts[host].Name, true
		}
		saved := p.room.save()
		if moves, ok := p.moveOff(stack, host, priority); ok {
			return moves, waitsForRoom(moves), true
		}
		p.room.restore(saved)
	}
	return nil, "", false
}

// movingOff returns the stacks that move off a host and have not stopped
// there yet.
func (p *planner) movingOff(host state.HostID) []string {
	var leaving []string
	for _, id := range slices.Sorted(maps.Keys(p.current.Stacks)) {
		if old, moving := p.room.stillOn(id); moving && old == host {
			leaving = append(leaving, id)
		}
	}
	return leaving
}

// moveOff moves stacks below priority off host, lowest first, until all of
// the stack fits there. ok is false if it never does.
func (p *planner) moveOff(stack *state.Stack, host state.HostID, priority int) ([]decision, bool) {
	var candidates []*state.Stack
	for _, id := range slices.Sorted(maps.Keys(p.current.Stacks)) {
		other := p.current.Stacks[id]
		switch {
		case other.Host == nil || *other.Host != host:
		case p.current.Locks[state.StackLock(id)] != 0:
		case !other.LastMove.IsZero() && p.now.Sub(other.LastMove) < MoveCooldown, p.failedRecently(other):
		case p.stackPriority(id) >= priority:
		default:
			candidates = append(candidates, other)
		}
	}
	slices.SortStableFunc(candidates, func(a, b *state.Stack) int {
		return cmp.Compare(p.stackPriority(a.ID()), p.stackPriority(b.ID()))
	})
	var moves []decision
	for _, other := range candidates {
		if len(moves) > 0 && p.fitsWhole(stack, host) {
			break
		}
		fit, ok := p.hostWithData(other, host)
		if !ok {
			continue
		}
		p.room.accept(other.ID(), fit)
		from := host
		moves = append(moves, decision{
			placement: Placement{Stack: other.ID()},
			move: &Move{
				Stack: other.ID(), From: &from, To: fit.host,
				Why: fmt.Sprintf("to make room for %s, of higher priority", stack.ID()),
			},
		})
	}
	return moves, len(moves) > 0 && p.fitsWhole(stack, host)
}

// fitsWhole tells whether all of a stack fits a host without pushing
// anything out.
func (p *planner) fitsWhole(stack *state.Stack, host state.HostID) bool {
	fit, reason := p.room.fitStack(stack.ID(), host)
	return reason == "" && len(fit.pushed) == 0 && len(fit.own) == 0
}

// hostWithData finds another host that has a stack's data and where all of
// the stack fits without pushing anything out. A stack that names a volume
// still waiting for its first stack stays, because the move would place
// that volume, and a stack moved to make room should change nothing else.
func (p *planner) hostWithData(stack *state.Stack, leftOut state.HostID) (stackFit, bool) {
	for _, host := range p.activeHosts(&leftOut) {
		if !p.fitsWhole(stack, host) || !p.hasData(stack, host) {
			continue
		}
		if volumes, _, _ := p.volumesOn(stack, host); len(volumes) == 0 {
			fit, _ := p.room.fitStack(stack.ID(), host)
			return fit, true
		}
	}
	return stackFit{}, false
}

// stackPriority returns a stack's highest effective priority, which the
// stack is placed with.
func (p *planner) stackPriority(id string) int {
	highest := 0
	for service := range p.current.Stacks[id].Services {
		highest = max(highest, p.room.priority(serviceRef{id, service}))
	}
	return highest
}

// waitsForRoom returns the reason for a stack that waits for the stacks
// that move to make room for it.
func waitsForRoom(moves []decision) string {
	var stacks []string
	for _, move := range moves {
		stacks = append(stacks, move.move.Stack)
	}
	return "waits for " + strings.Join(stacks, ", ") + " to move and make room"
}

// failedRecently tells whether a move of the stack failed within
// MoveCooldown. Until then, the placement does not start a move of its own
// again, so a move that cannot work does not resync copies over and over.
func (p *planner) failedRecently(stack *state.Stack) bool {
	return !stack.MoveFailed.IsZero() && p.now.Sub(stack.MoveFailed) < MoveCooldown
}
