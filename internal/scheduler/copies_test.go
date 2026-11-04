package scheduler

import (
	"slices"
	"strings"
	"testing"

	"github.com/webcodingcat/clowder/internal/resource"
	"github.com/webcodingcat/clowder/internal/state"
)

// hosts is cluster() with hosts 1 to last, active and with host 0's pools.
func hosts(last state.HostID) *state.State {
	current := cluster()
	for id := state.HostID(1); id <= last; id++ {
		current.Hosts[id] = &state.Host{
			ID: id, Name: "node" + string(rune('0'+id)), Phase: state.Active,
			Pools: current.Hosts[0].Pools,
		}
	}
	return current
}

// runningShop places shop with the volume data (replicas as given) on host
// 0, with the copies given, each synced.
func runningShop(current *state.State, replicas int, copies ...state.VolumeCopy) *state.Volume {
	shop := stack(current, "shop", map[string]resource.VolumeSpec{"data": volume("fast", 10, replicas)})
	host := state.HostID(0)
	shop.Host, shop.Status = &host, state.StackRunning
	data := current.Volumes["default/data"]
	minor := 0
	data.Status, data.Minor = state.VolumeOK, &minor
	for _, volumeCopy := range copies {
		volumeCopy.Class, volumeCopy.Synced = "fast", !volumeCopy.Tiebreaker
		data.Copies = append(data.Copies, volumeCopy)
	}
	return data
}

// hostsOf lists the hosts of a record's copies, a tiebreaker as "t<host>".
func hostsOf(volume state.Volume) []string {
	var listed []string
	for _, volumeCopy := range volume.Copies {
		name := string(rune('0' + volumeCopy.Host))
		if volumeCopy.Tiebreaker {
			name = "t" + name
		}
		listed = append(listed, name)
	}
	slices.Sort(listed)
	return listed
}

func TestAMissingCopyGoesToTheHostWithTheMostFreeSpace(t *testing.T) {
	current := hosts(2)
	runningShop(current, 2, state.VolumeCopy{Host: 0})
	current.Hosts[1].Pools = map[string]state.Pool{"vg-fast/thin": {SizeBytes: 50 * gib}}
	copies := Plan(current, planTime).Copies
	// with the tiebreaker the two copies need, in the same record
	if len(copies) != 1 || !slices.Equal(hostsOf(copies[0]), []string{"0", "2", "t1"}) {
		t.Fatalf("copies %+v", copies)
	}
	if added := copies[0].Copies[1]; added.Synced {
		t.Errorf("a new copy is synced: %+v", added)
	}
}

func TestOneCopySyncsAtATimeInTheWholeCluster(t *testing.T) {
	current := hosts(1)
	runningShop(current, 2, state.VolumeCopy{Host: 0})
	blog := stack(current, "blog", map[string]resource.VolumeSpec{"files": volume("fast", 10, 2)})
	host := state.HostID(1)
	blog.Host, blog.Status = &host, state.StackRunning
	files := current.Volumes["default/files"]
	minor := 1
	files.Status, files.Minor, files.Copies = state.VolumeOK, &minor, []state.VolumeCopy{{Host: 1, Class: "fast", Synced: true}}
	if copies := Plan(current, planTime).Copies; len(copies) != 1 || copies[0].Name != "data" {
		t.Fatalf("two copies at once: %+v", copies)
	}
	// while data's new copy syncs, files waits
	data := current.Volumes["default/data"]
	data.Copies = append(data.Copies, state.VolumeCopy{Host: 1, Class: "fast"})
	if copies := Plan(current, planTime).Copies; len(copies) != 0 {
		t.Errorf("a copy while another syncs: %+v", copies)
	}
	data.Copies[1].Synced = true
	copies := Plan(current, planTime).Copies
	if len(copies) != 1 || copies[0].Name != "files" || !slices.Equal(hostsOf(copies[0]), []string{"0", "1"}) {
		t.Errorf("once synced, files gets its copy: %+v", copies)
	}
}

func TestAVolumeWithTwoCopiesGetsATiebreakerOnceThereAreThreeHosts(t *testing.T) {
	current := hosts(1)
	runningShop(current, 2, state.VolumeCopy{Host: 0}, state.VolumeCopy{Host: 1})
	if copies := Plan(current, planTime).Copies; len(copies) != 0 {
		t.Fatalf("a tiebreaker with two hosts: %+v", copies)
	}
	current = hosts(3)
	runningShop(current, 2, state.VolumeCopy{Host: 0}, state.VolumeCopy{Host: 1})
	// host 2 holds another volume's tiebreaker already
	other := stack(current, "blog", map[string]resource.VolumeSpec{"files": volume("fast", 1, 2)})
	other.Status = state.StackPending
	files := current.Volumes["default/files"]
	minor := 1
	files.Status, files.Minor = state.VolumeOK, &minor
	files.Copies = []state.VolumeCopy{
		{Host: 0, Class: "fast", Synced: true},
		{Host: 1, Class: "fast", Synced: true},
		{Host: 2, Tiebreaker: true},
	}
	copies := Plan(current, planTime).Copies
	if len(copies) != 1 || !slices.Equal(hostsOf(copies[0]), []string{"0", "1", "t3"}) {
		t.Fatalf("copies %+v", copies)
	}
	// a tiebreaker on a host that leaves moves
	current.Volumes["default/data"].Copies = copies[0].Copies
	current.Hosts[3].Phase, current.Hosts[3].Leaving = state.Draining, true
	if copies := Plan(current, planTime).Copies; len(copies) != 1 || !slices.Equal(hostsOf(copies[0]), []string{"0", "1", "t2"}) {
		t.Errorf("copies %+v", copies)
	}
}

func TestASurplusCopyGoesOnceTheOthersAreSyncedButNotWhereItsStackRuns(t *testing.T) {
	current := hosts(2)
	data := runningShop(current, 1, state.VolumeCopy{Host: 0}, state.VolumeCopy{Host: 1})
	data.Copies[1].Synced = false
	// the copy that syncs goes: the other one is synced
	if copies := Plan(current, planTime).Copies; len(copies) != 1 || !slices.Equal(hostsOf(copies[0]), []string{"0"}) {
		t.Fatalf("copies %+v", copies)
	}
	data.Copies[0].Synced, data.Copies[1].Synced = false, true
	// neither goes: the stack's own copy, nor one that leaves an unsynced
	// copy alone; meanwhile the two copies get a tiebreaker
	if copies := Plan(current, planTime).Copies; len(copies) != 1 || !slices.Equal(hostsOf(copies[0]), []string{"0", "1", "t2"}) {
		t.Fatalf("copies %+v", copies)
	}
	// both synced: the one beside the stack's goes
	data.Copies[0].Synced = true
	if copies := Plan(current, planTime).Copies; len(copies) != 1 || !slices.Equal(hostsOf(copies[0]), []string{"0"}) {
		t.Errorf("copies %+v", copies)
	}
}

func TestACopyOnALeavingHostIsReplacedAndThenGivenUp(t *testing.T) {
	current := hosts(2)
	runningShop(current, 2, state.VolumeCopy{Host: 0}, state.VolumeCopy{Host: 1})
	current.Hosts[1].Phase, current.Hosts[1].Leaving = state.Draining, true
	copies := Plan(current, planTime).Copies
	if len(copies) != 1 || !slices.Equal(hostsOf(copies[0]), []string{"0", "1", "2"}) {
		t.Fatalf("copies %+v", copies)
	}
	current.Volumes["default/data"].Copies = copies[0].Copies
	current.Volumes["default/data"].Copies[2].Synced = true
	if copies := Plan(current, planTime).Copies; len(copies) != 1 || !slices.Equal(hostsOf(copies[0]), []string{"0", "2"}) {
		t.Errorf("copies %+v", copies)
	}
}

func TestAVolumeAnOperationHoldsIsLeftToIt(t *testing.T) {
	current := hosts(2)
	runningShop(current, 2, state.VolumeCopy{Host: 0})
	current.Locks = map[string]uint64{"volume/default/data": 7, "stack/default/shop": 7}
	current.Hosts[0].Phase = state.Draining
	decisions := Plan(current, planTime)
	if len(decisions.Copies) != 0 || len(decisions.Moves) != 0 || len(decisions.Waiting) != 0 {
		t.Errorf("decisions %+v", decisions)
	}
}

func TestAStackMovesOffADrainingHostToOneWithItsDataFirst(t *testing.T) {
	current := hosts(2)
	runningShop(current, 2, state.VolumeCopy{Host: 0}, state.VolumeCopy{Host: 2})
	current.Hosts[0].Phase = state.Draining
	moves := Plan(current, planTime).Moves
	if len(moves) != 1 || *moves[0].From != 0 || moves[0].To != 2 || len(firstStep(t, current, moves[0])) != 0 {
		t.Fatalf("moves %+v", moves)
	}
	// without a copy anywhere else, one comes with the move
	current = hosts(1)
	runningShop(current, 1, state.VolumeCopy{Host: 0})
	current.Hosts[0].Phase = state.Draining
	moves = Plan(current, planTime).Moves
	if len(moves) != 1 || moves[0].To != 1 || len(firstStep(t, current, moves[0])) != 1 ||
		!slices.Equal(hostsOf(firstStep(t, current, moves[0])[0]), []string{"0", "1"}) {
		t.Fatalf("moves %+v", moves)
	}
	// but not while another copy syncs
	current.Volumes["default/data"].Copies[0].Synced = false
	waiting := Plan(current, planTime).Waiting
	if len(waiting) != 1 || !strings.Contains(waiting[0].Reason, "another copy syncs already") {
		t.Errorf("waiting %+v", waiting)
	}
}

func TestAStackMovesWhereAUserAsks(t *testing.T) {
	current := hosts(2)
	runningShop(current, 2, state.VolumeCopy{Host: 0}, state.VolumeCopy{Host: 1}, state.VolumeCopy{Host: 2, Tiebreaker: true})
	to := state.HostID(2)
	current.Stacks["default/shop"].MoveTo = &to
	moves := Plan(current, planTime).Moves
	if len(moves) != 1 || moves[0].To != 2 || len(firstStep(t, current, moves[0])) != 1 ||
		!slices.Equal(hostsOf(firstStep(t, current, moves[0])[0]), []string{"0", "1", "2"}) {
		t.Fatalf("the tiebreaker gets a disk: %+v", moves)
	}
	current.Hosts[2].Phase = state.Draining
	waiting := Plan(current, planTime).Waiting
	if len(waiting) != 1 || !strings.Contains(waiting[0].Reason, "cannot move (a user asked to move it)") {
		t.Errorf("waiting %+v", waiting)
	}
}

// firstStep is what the first step of a move places: the records of its
// volumes with a copy on the new host.
func firstStep(t *testing.T, current *state.State, move Move) []state.Volume {
	t.Helper()
	volumes, reason := MoveVolumes(current, move.Stack, move.To)
	if reason != "" {
		t.Fatalf("the first step of %+v: %s", move, reason)
	}
	return volumes
}

func TestAThrowAwayVolumeLeavesALeavingHostAnewOnlyWithoutItsStack(t *testing.T) {
	current := hosts(1)
	data := runningShop(current, 0, state.VolumeCopy{Host: 1})
	data.Minor = nil
	current.Hosts[1].Phase, current.Hosts[1].Leaving = state.Draining, true
	// without its stack, data goes anew to host 0
	shop := current.Stacks["default/shop"]
	delete(current.Stacks, "default/shop")
	copies := Plan(current, planTime).Copies
	if len(copies) != 1 || copies[0].DataGeneration != 2 || !slices.Equal(hostsOf(copies[0]), []string{"0"}) {
		t.Fatalf("copies %+v", copies)
	}
	// with its stack on the leaving host, it moves with the stack
	one := state.HostID(1)
	shop.Host = &one
	current.Stacks["default/shop"] = shop
	for _, record := range Plan(current, planTime).Copies {
		if record.Name == "data" {
			t.Errorf("the volume left before its stack: %+v", record)
		}
	}
}

func TestAFailedMoveIsNotTriedAgainWithinTheCooldownUnlessAUserAsks(t *testing.T) {
	current := hosts(2)
	runningShop(current, 2, state.VolumeCopy{Host: 0}, state.VolumeCopy{Host: 2})
	current.Hosts[0].Phase = state.Draining
	shop := current.Stacks["default/shop"]
	shop.MoveFailed = planTime.Add(-MoveCooldown / 2)
	decisions := Plan(current, planTime)
	if len(decisions.Moves) != 0 || len(decisions.Waiting) != 1 || !strings.Contains(decisions.Waiting[0].Reason, "a move failed") {
		t.Fatalf("moves %+v, waiting %+v", decisions.Moves, decisions.Waiting)
	}
	to := state.HostID(1)
	current.Hosts[0].Phase = state.Active
	shop.MoveTo = &to
	if moves := Plan(current, planTime).Moves; len(moves) != 1 {
		t.Errorf("a user's request waits for the cooldown: %+v", moves)
	}
	shop.MoveTo = nil
	current.Hosts[0].Phase = state.Draining
	if moves := Plan(current, planTime.Add(MoveCooldown)).Moves; len(moves) != 1 {
		t.Errorf("after the cooldown: %+v", moves)
	}
}

// After a move the old copy becomes the tiebreaker, in the record that
// gives it up: no node leaves the resource, so no window opens in which
// the two copies left have no third vote.
func TestTheCopyAMoveGivesUpBecomesTheTiebreaker(t *testing.T) {
	current := hosts(2)
	runningShop(current, 2, state.VolumeCopy{Host: 0}, state.VolumeCopy{Host: 1}, state.VolumeCopy{Host: 2})
	// the stack moved from host 1 to host 0, whose tiebreaker got a disk
	copies := Plan(current, planTime).Copies
	if len(copies) != 1 || !slices.Equal(hostsOf(copies[0]), []string{"0", "1", "t2"}) {
		t.Fatalf("copies %+v", copies)
	}
	if tiebreaker := copies[0].Copies[2]; tiebreaker.Class != "fast" {
		t.Errorf("the tiebreaker forgot the class of its LV: %+v", tiebreaker)
	}
	// a host that leaves gives its copy up, and the tiebreaker goes elsewhere
	// in the same record
	current = hosts(3)
	runningShop(current, 2, state.VolumeCopy{Host: 0}, state.VolumeCopy{Host: 1}, state.VolumeCopy{Host: 2})
	current.Hosts[2].Phase, current.Hosts[2].Leaving = state.Draining, true
	copies = Plan(current, planTime).Copies
	if len(copies) != 1 || !slices.Equal(hostsOf(copies[0]), []string{"0", "1", "t3"}) {
		t.Fatalf("copies %+v", copies)
	}
}

// A volume of replicas 1 that moves has two copies for a while: they get a
// tiebreaker with the new copy, and lose it with the old one.
func TestAMoveOfOneReplicaHasATiebreakerWhileItLasts(t *testing.T) {
	current := hosts(2)
	runningShop(current, 1, state.VolumeCopy{Host: 0})
	shop := current.Stacks["default/shop"]
	to := state.HostID(1)
	shop.MoveTo = &to
	moves := Plan(current, planTime).Moves
	if len(moves) != 1 {
		t.Fatalf("moves %+v", moves)
	}
	moved := firstStep(t, current, moves[0])
	if len(moved) != 1 || !slices.Equal(hostsOf(moved[0]), []string{"0", "1", "t2"}) {
		t.Fatalf("the move's copies %+v", moved)
	}
	current.Volumes["default/data"].Copies = moved[0].Copies
	current.Volumes["default/data"].Copies[1].Synced = true
	host := state.HostID(1)
	shop.Host, shop.MoveTo = &host, nil
	copies := Plan(current, planTime).Copies
	if len(copies) != 1 || !slices.Equal(hostsOf(copies[0]), []string{"1"}) {
		t.Errorf("copies %+v", copies)
	}
}

// Two records of one volume in a plan (a move's and the copies') count
// its tiebreaker once.
func TestAVolumesTiebreakerCountsOncePerPlan(t *testing.T) {
	current := hosts(3)
	data := runningShop(current, 2, state.VolumeCopy{Host: 0}, state.VolumeCopy{Host: 1}, state.VolumeCopy{Host: 2, Tiebreaker: true})
	p := newPlanner(current)
	moved := *data
	moved.Copies = []state.VolumeCopy{{Host: 0}, {Host: 1}, {Host: 3, Tiebreaker: true}}
	p.countTiebreakers(moved)
	p.countTiebreakers(moved)
	if p.tiebreakers[2] != 0 || p.tiebreakers[3] != 1 {
		t.Errorf("tiebreakers %v", p.tiebreakers)
	}
}
