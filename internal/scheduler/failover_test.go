package scheduler

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/webcodingcat/clowder/internal/state"
)

// fenced makes host 0, where runningShop places shop, fenced at planTime.
func fenced(current *state.State) {
	current.Hosts[0].Phase = state.Fenced
	current.Hosts[0].FencedAt = []time.Time{planTime}
}

func TestAStackFailsOverToAHostWithASyncedCopy(t *testing.T) {
	current := hosts(2)
	runningShop(current, 2, state.VolumeCopy{Host: 0}, state.VolumeCopy{Host: 2}, state.VolumeCopy{Host: 1, Tiebreaker: true})
	fenced(current)
	decisions := Plan(current, planTime)
	if len(decisions.Moves) != 0 || len(decisions.Placements) != 1 {
		t.Fatalf("decisions %+v", decisions)
	}
	if placed := decisions.Placements[0]; !placed.SetsHost || placed.Host != 2 || len(placed.Volumes) != 0 {
		t.Errorf("placement %+v", placed)
	}
}

func TestAStackWaitsWhileItsHostOnlyFailed(t *testing.T) {
	current := hosts(2)
	runningShop(current, 2, state.VolumeCopy{Host: 0}, state.VolumeCopy{Host: 2})
	for _, phase := range []state.HostPhase{state.Failed, state.Fencing} {
		current.Hosts[0].Phase = phase
		if decisions := Plan(current, planTime); len(decisions.Placements) != 0 || len(decisions.Moves) != 0 {
			t.Errorf("%s: decisions %+v", phase, decisions)
		}
	}
}

func TestAStackWithoutASyncedCopyElsewhereWaitsForItsHost(t *testing.T) {
	current := hosts(2)
	runningShop(current, 1, state.VolumeCopy{Host: 0})
	fenced(current)
	waiting := Plan(current, planTime).Waiting
	if len(waiting) != 1 || !strings.Contains(waiting[0].Reason, "no synced copy of volume data") {
		t.Fatalf("waiting %+v", waiting)
	}
	// a copy that has not caught up is not enough either
	current = hosts(2)
	runningShop(current, 2, state.VolumeCopy{Host: 0}, state.VolumeCopy{Host: 2})
	current.Volumes["default/data"].Copies[1].Synced = false
	fenced(current)
	if waiting := Plan(current, planTime).Waiting; len(waiting) != 1 {
		t.Errorf("waiting %+v", waiting)
	}
}

func TestAThrowAwayVolumeStartsAnewWhereItsStackFailsOver(t *testing.T) {
	current := hosts(1)
	data := runningShop(current, 0, state.VolumeCopy{Host: 0})
	data.Minor = nil
	fenced(current)
	placements := Plan(current, planTime).Placements
	if len(placements) != 1 || placements[0].Host != 1 || len(placements[0].Volumes) != 1 {
		t.Fatalf("placements %+v", placements)
	}
	if fresh := placements[0].Volumes[0]; fresh.DataGeneration != 2 || !slices.Equal(hostsOf(fresh), []string{"1"}) {
		t.Errorf("volume %+v", fresh)
	}
}

func TestAFencedHostsCopiesCountForAWhile(t *testing.T) {
	// a fourth host, so that three take tiebreakers while host 0 is fenced
	current := hosts(3)
	runningShop(current, 2, state.VolumeCopy{Host: 0}, state.VolumeCopy{Host: 2}, state.VolumeCopy{Host: 3, Tiebreaker: true})
	fenced(current)
	current.Volumes["default/data"].Copies[0].Synced = false
	two := state.HostID(2)
	current.Stacks["default/shop"].Host = &two // failed over already
	if copies := Plan(current, planTime.Add(CopyWait-time.Minute)).Copies; len(copies) != 0 {
		t.Errorf("a copy replaced while its host may come back: %+v", copies)
	}
	copies := Plan(current, planTime.Add(CopyWait)).Copies
	if len(copies) != 1 || !slices.Equal(hostsOf(copies[0]), []string{"0", "1", "2", "t3"}) {
		t.Fatalf("copies %+v", copies)
	}
	// a copy on a host that is down takes no resync limit
	if newPlanner(current).syncing {
		t.Error("the fenced host's copy takes the resync limit")
	}
	// a disabled host's copies count no more at all
	current.Hosts[0].Phase = state.Disabled
	if copies := Plan(current, planTime).Copies; len(copies) != 1 {
		t.Errorf("copies %+v", copies)
	}
}

func TestARecoveringHostGivesUpTheCopyThatWasReplaced(t *testing.T) {
	current := hosts(2)
	runningShop(current, 2, state.VolumeCopy{Host: 0}, state.VolumeCopy{Host: 1}, state.VolumeCopy{Host: 2})
	current.Hosts[0].Phase = state.Recovering
	current.Volumes["default/data"].Copies[0].Synced = false
	two := state.HostID(2)
	current.Stacks["default/shop"].Host = &two
	copies := Plan(current, planTime).Copies
	if len(copies) != 1 || !slices.Equal(hostsOf(copies[0]), []string{"1", "2"}) {
		t.Errorf("copies %+v", copies)
	}
}

func TestAMinorStaysTakenWhileARetiredCopyHasIt(t *testing.T) {
	current := hosts(1)
	current.RetiredMinors = map[int]string{0: "default/gone"}
	if minor, ok := newPlanner(current).freeMinor(nil); !ok || minor != 1 {
		t.Errorf("minor %d (%v), want 1", minor, ok)
	}
}
