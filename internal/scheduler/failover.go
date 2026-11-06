package scheduler

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/state"
)

// CopyWait is how long, from the fence on, a fenced host's copies still
// count toward their volumes' replicas. The leader powers the host on again
// once its fence is over (at most state.StepTime later), and the host is
// usually back (recovering) within minutes after that, to catch its copies
// up. A host that stays off longer, or is disabled, gets its copies made
// anew elsewhere.
const CopyWait = state.StepTime + 10*time.Minute

// failover places a stack whose host was fenced on another host at once,
// without a move, because the old host is off and stops nothing. Only a
// host with a synced copy of each of the stack's volumes of replicas 1 or
// more can take it, as nothing can sync from the old host. A volume of
// replicas 0 starts anew there, empty (a new data generation). A stack
// that finds no such host waits on its old host. With replicas 1, it waits
// until the host is back or a user restores the volume elsewhere.
func (p *planner) failover(stack *state.Stack) []decision {
	from := *stack.Host
	reasons := map[state.HostID]string{}
	// whole are the hosts where all of the stack fits, and pushing are
	// those where it pushes services out (stackFit), as in place
	var whole, pushing []stackFit
	volumes := map[state.HostID][]state.Volume{}
	for _, id := range p.activeHosts(&from) {
		fresh, reason := p.failoverVolumes(stack, id)
		var fit stackFit
		if reason == "" {
			fit, reason = p.room.fitStack(stack.ID(), id)
		}
		switch {
		case reason != "":
			reasons[id] = reason
		case len(fit.pushed) == 0 && len(fit.own) == 0:
			whole, volumes[id] = append(whole, fit), fresh
		default:
			pushing, volumes[id] = append(pushing, fit), fresh
		}
	}
	freeMemory := map[state.HostID]int64{}
	for _, fit := range whole {
		freeMemory[fit.host] = p.room.free(fit.host).memory
	}
	// best fit, as for any placement; one that pushes others out last
	slices.SortStableFunc(whole, func(a, b stackFit) int { return cmp.Compare(freeMemory[a.host], freeMemory[b.host]) })
	if chosen := slices.Concat(whole, pushing); len(chosen) > 0 {
		host := chosen[0].host
		pushedOut := p.room.accept(stack.ID(), chosen[0])
		p.reserve(host, volumes[host])
		return []decision{{placement: Placement{
			Stack: stack.ID(), Host: host, SetsHost: true, Volumes: volumes[host], PushedOut: pushedOut,
		}}}
	}
	reason := "its host " + p.current.Hosts[from].Name + " is down, and no active host can take it"
	if len(reasons) > 0 {
		var each []string
		for _, id := range slices.Sorted(maps.Keys(reasons)) {
			each = append(each, p.current.Hosts[id].Name+": "+reasons[id])
		}
		reason += ": " + strings.Join(each, "; ")
	}
	return []decision{{placement: Placement{Stack: stack.ID()}, reason: reason}}
}

// failoverVolumes works out what a stack's volumes need for the stack to
// fail over to host. A volume of replicas 1 or more must have a synced copy
// with a disk there. A volume of replicas 0 gets a new, empty LV there,
// because its old LV is on the host that is down. It returns the new
// records of the volumes of replicas 0, or why host cannot take the stack.
func (p *planner) failoverVolumes(stack *state.Stack, host state.HostID) ([]state.Volume, string) {
	var fresh []state.Volume
	var reserved []allocation
	for _, name := range stack.Spec.VolumeNames() {
		volume, ok := p.current.Volumes[names.ID(stack.Namespace, name)]
		switch {
		case !ok || volume.Status == state.VolumePending:
			return nil, fmt.Sprintf("volume %s is not placed", name) // a placed stack's volumes are; a guard
		case volume.Spec.Replicas > 0:
			if !slices.ContainsFunc(volume.Copies, func(c state.VolumeCopy) bool {
				return c.Host == host && !c.Tiebreaker && c.Synced
			}) {
				return nil, fmt.Sprintf("no synced copy of volume %s", name)
			}
			continue
		case slices.ContainsFunc(volume.Copies, func(c state.VolumeCopy) bool { return c.Host == host }):
			continue
		}
		class, pool, ok := p.fit(host, volume.Spec.Class, int64(volume.Spec.Size), reserved)
		if !ok {
			return nil, noPool(name, volume.Spec)
		}
		if p.retiresHere(volume.ID(), volume.DataGeneration+1, host) {
			return nil, fmt.Sprintf("volume %s: the host still removes an old copy", name)
		}
		reserved = append(reserved, allocation{pool, int64(volume.Spec.Size)})
		record := *volume
		record.DataGeneration++
		record.Copies = []state.VolumeCopy{{Host: host, Class: class}}
		fresh = append(fresh, record)
	}
	return fresh, ""
}
