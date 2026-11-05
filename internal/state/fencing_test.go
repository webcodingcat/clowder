package state

import (
	"slices"
	"testing"
	"time"
)

// failedHost makes a cluster of three active hosts in which host 1 runs
// default/shop, has a synced copy of default/db, and failed.
func failedHost(t *testing.T) *cluster {
	t.Helper()
	c := newCluster(t)
	for id := HostID(1); id <= 2; id++ {
		c.addHost(testHost(id))
		c.mustApply(KindSetHostPhase, LeaderActor, SetHostPhase{Host: id, Phase: Active})
	}
	c.putShop(shopSpec())
	one := HostID(1)
	c.mustApply(KindSetStackStatus, LeaderActor,
		SetStackStatus{PlannedAt: c.index, ID: "default/shop", Status: StackStarting, Host: &one})
	c.state.Volumes["default/db"] = &Volume{
		Meta: Meta{Namespace: "default", Name: "db"}, Status: VolumeOK,
		Copies: []VolumeCopy{{Host: 1, Class: "fast", Synced: true}, {Host: 2, Class: "fast", Synced: true}},
	}
	c.mustApply(KindSetHostPhase, LeaderActor, SetHostPhase{Host: 1, Phase: Failed, From: Active})
	return c
}

// startFence starts the fence-host operation of host 1 and returns its ID.
func (c *cluster) startFence() uint64 {
	c.t.Helper()
	c.mustApply(KindStartOperation, LeaderActor, StartOperation{
		Kind: OperationFenceHost, Locks: []string{HostLock(1)}, Fence: &HostFence{Host: 1},
	})
	return c.index
}

func TestAFailedHostThatIsBackBeforeTheFenceIsNotFenced(t *testing.T) {
	c := failedHost(t)
	// the host itself cannot say it is back
	c.refuse(KindSetHostPhase, HostActor(1), SetHostPhase{Host: 1, Phase: Active, From: Failed}, "only the leader")
	c.mustApply(KindSetHostPhase, LeaderActor, SetHostPhase{Host: 1, Phase: Active, From: Failed})
	// the fence the leader planned on the old state comes too late
	c.refuse(KindStartOperation, LeaderActor, StartOperation{
		Kind: OperationFenceHost, Locks: []string{HostLock(1)}, Fence: &HostFence{Host: 1},
	}, "not failed")
	if len(c.state.Locks) != 0 {
		t.Errorf("locks %v", c.state.Locks)
	}
}

func TestAFenceOnceStartedIsNotStoppedByTheHostComingBack(t *testing.T) {
	c := failedHost(t)
	id := c.startFence()
	if phase := c.state.Hosts[1].Phase; phase != Fencing {
		t.Fatalf("phase %s, want fencing with the operation's start", phase)
	}
	// the beats came back after the start: neither the leader's late
	// decision nor the host stops the fence
	c.refuse(KindSetHostPhase, LeaderActor, SetHostPhase{Host: 1, Phase: Active, From: Failed}, "no longer failed")
	c.refuse(KindSetHostPhase, LeaderActor, SetHostPhase{Host: 1, Phase: Failed}, "locked by operation")
	c.refuse(KindSetHostPhase, HostActor(1), SetHostPhase{Host: 1, Phase: Active}, "locked by operation")
	// the operation cannot end while the host is fencing
	c.refuse(KindFinishOperation, LeaderActor, FinishOperation{ID: id}, "still fencing")

	c.mustApply(KindSetHostPhase, LeaderActor, SetHostPhase{Host: 1, Phase: Fenced, From: Fencing, Operation: id})
	host := c.state.Hosts[1]
	if host.Phase != Fenced || !slices.Equal(host.FencedAt, []time.Time{testTime}) {
		t.Errorf("host %s, fenced at %v", host.Phase, host.FencedAt)
	}
	if stacks := c.state.Operations[id].Fence.Stacks; !slices.Equal(stacks, []string{"default/shop"}) {
		t.Errorf("the fence waits for %v", stacks)
	}
	if copies := c.state.Volumes["default/db"].Copies; copies[0].Synced || !copies[1].Synced {
		t.Errorf("copies %+v: only the fenced host's is not synced any more", copies)
	}
	c.mustApply(KindAdvanceOperation, LeaderActor, AdvanceOperation{ID: id, FromStep: FenceStart, Step: FenceOff})
	c.mustApply(KindFinishOperation, LeaderActor, FinishOperation{ID: id})
	if c.state.HostLocked(1) {
		t.Error("the host stays locked")
	}
}

func TestAFenceThatCannotPowerOffTakesTheHostBackToFailed(t *testing.T) {
	c := failedHost(t)
	id := c.startFence()
	c.refuse(KindSetHostPhase, LeaderActor, SetHostPhase{Host: 1, Phase: Failed, From: Fencing}, "locked by operation")
	c.mustApply(KindSetHostPhase, LeaderActor, SetHostPhase{Host: 1, Phase: Failed, From: Fencing, Operation: id})
	c.mustApply(KindFinishOperation, LeaderActor, FinishOperation{ID: id, Failed: true, Error: "the API is down"})
	if host := c.state.Hosts[1]; host.Phase != Failed || len(host.FencedAt) != 0 {
		t.Errorf("host %s, fenced at %v", host.Phase, host.FencedAt)
	}
	// a user who checked that it is off confirms it
	c.mustApply(KindSetHostPhase, UserActor("alice"), SetHostPhase{Host: 1, Phase: Fenced, From: Failed})
	if host := c.state.Hosts[1]; host.Phase != Fenced || len(host.FencedAt) != 1 {
		t.Errorf("host %s, fenced at %v", host.Phase, host.FencedAt)
	}
}

func TestOnlyAFailedHostWithACheckedServerIsFenced(t *testing.T) {
	c := failedHost(t)
	c.refuse(KindStartOperation, LeaderActor, StartOperation{
		Kind: OperationFenceHost, Locks: []string{HostLock(2)}, Fence: &HostFence{Host: 1},
	}, "must lock it")
	c.refuse(KindStartOperation, LeaderActor, StartOperation{
		Kind: OperationFenceHost, Locks: []string{HostLock(2)}, Fence: &HostFence{Host: 2},
	}, "not failed")
	c.refuse(KindStartOperation, LeaderActor, StartOperation{
		Kind: "drain-host", Locks: []string{HostLock(1)}, Fence: &HostFence{Host: 1},
	}, "a fence goes with fence-host")
	c.state.Hosts[1].ServerChecked = false
	c.refuse(KindStartOperation, LeaderActor, StartOperation{
		Kind: OperationFenceHost, Locks: []string{HostLock(1)}, Fence: &HostFence{Host: 1},
	}, "no checked server")
}

func TestAFencedHostIsBackOnceItsCopiesCaughtUp(t *testing.T) {
	c := failedHost(t)
	c.mustApply(KindSetHostPhase, UserActor("alice"), SetHostPhase{Host: 1, Phase: Fenced, From: Failed})
	c.mustApply(KindSetHostPhase, HostActor(1), SetHostPhase{Host: 1, Phase: Recovering, From: Fenced})
	c.refuse(KindSetHostPhase, HostActor(1), SetHostPhase{Host: 1, Phase: Active}, "not synced yet")
	if c.state.CaughtUp(1) {
		t.Error("caught up with a copy not synced")
	}
	c.state.Volumes["default/db"].Copies[0].Synced = true
	c.mustApply(KindSetHostPhase, HostActor(1), SetHostPhase{Host: 1, Phase: Active, From: Recovering})
}

func TestTheFlappingBrakeCountsTheFencesOfAnHour(t *testing.T) {
	host := &Host{}
	start := testTime
	for _, minutes := range []int{0, 20, 40} {
		host.FencedAt = append(host.FencedAt, start.Add(time.Duration(minutes)*time.Minute))
	}
	if !host.Flapping(start.Add(50 * time.Minute)) {
		t.Error("three fences within an hour do not brake")
	}
	if host.Flapping(start.Add(61 * time.Minute)) {
		t.Error("a fence an hour ago still counts")
	}

	c := failedHost(t)
	c.state.Hosts[1].FencedAt = host.FencedAt
	c.state.Hosts[1].Phase = Disabled
	c.mustApply(KindSetHostPhase, UserActor("alice"), SetHostPhase{Host: 1, Phase: Fenced, From: Disabled})
	if fenced := c.state.Hosts[1].FencedAt; len(fenced) != 0 {
		t.Errorf("enabled with the fences %v still counted", fenced)
	}
}

func TestAFenceForgetsFencesOlderThanTheBrakesWindow(t *testing.T) {
	c := failedHost(t)
	c.state.Hosts[1].FencedAt = []time.Time{testTime.Add(-2 * time.Hour), testTime.Add(-30 * time.Minute)}
	c.mustApply(KindSetHostPhase, UserActor("alice"), SetHostPhase{Host: 1, Phase: Fenced, From: Failed})
	want := []time.Time{testTime.Add(-30 * time.Minute), testTime}
	if fenced := c.state.Hosts[1].FencedAt; !slices.Equal(fenced, want) {
		t.Errorf("fenced at %v, want %v", fenced, want)
	}
}

func TestAStackFailsOverOnlyFromAHostThatIsOff(t *testing.T) {
	c := failedHost(t)
	two := HostID(2)
	failover := SetStackStatus{ID: "default/shop", Status: StackStarting, Host: &two}
	// failed: it may still run the stack
	failover.PlannedAt = c.index
	c.refuse(KindSetStackStatus, LeaderActor, failover, "only an operation that holds its lock moves it")
	c.mustApply(KindSetHostPhase, UserActor("alice"), SetHostPhase{Host: 1, Phase: Fenced, From: Failed})
	// only to an active host
	c.state.Hosts[2].Phase = Draining
	failover.PlannedAt = c.index
	c.refuse(KindSetStackStatus, LeaderActor, failover, "it is draining")
	c.state.Hosts[2].Phase = Active
	c.mustApply(KindSetStackStatus, LeaderActor, failover)
	if host := c.state.Stacks["default/shop"].Host; host == nil || *host != 2 {
		t.Errorf("the stack is on host %v", host)
	}
}

func TestAHostKeepsItsStacksUntilItIsFenced(t *testing.T) {
	for phase, keeps := range map[HostPhase]bool{
		Active: true, Draining: true, Failed: true, Fencing: true,
		Fenced: false, Recovering: false, Disabled: false, Joining: false,
	} {
		if KeepsStacks(phase) != keeps {
			t.Errorf("%s keeps its stacks: %v", phase, !keeps)
		}
	}
}

func TestAFailureKeepsAUsersRemove(t *testing.T) {
	c := failedHost(t)
	c.state.Hosts[1].Phase, c.state.Hosts[1].Leaving = Draining, true
	c.mustApply(KindSetHostPhase, LeaderActor, SetHostPhase{Host: 1, Phase: Failed, From: Draining})
	c.mustApply(KindSetHostPhase, LeaderActor, SetHostPhase{Host: 1, Phase: Active, From: Failed})
	if !c.state.Hosts[1].Leaving {
		t.Error("the failure took the remove back")
	}
}
