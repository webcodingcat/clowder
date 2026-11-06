package state

import (
	"errors"
	"testing"
	"time"

	"github.com/webcodingcat/clowder/internal/hostrole"
	"github.com/webcodingcat/clowder/internal/resource"
	"github.com/webcodingcat/clowder/internal/stackspec"
)

// moveCluster is lockCluster with default/data as default/shop's volume,
// replicated twice, and its copy on host 0 synced.
func moveCluster(t *testing.T) *cluster {
	t.Helper()
	c := newCluster(t)
	c.configure(testConfig())
	c.activeHosts(2)
	spec := testVolumeSpec()
	spec.Replicas = 2
	c.put(resource.KindVolume, "default", "data", spec)
	shop := shopSpec()
	app := shop.Services["app"]
	app.Volumes = []stackspec.Mount{{Volume: "data", Path: "/data"}}
	shop.Services["app"] = app
	c.putShop(shop)
	c.mustApply(KindPutVolume, LeaderActor, PutVolume{PlannedAt: c.index, Volume: c.placedData()})
	host := HostID(0)
	c.mustApply(KindSetStackStatus, LeaderActor, SetStackStatus{PlannedAt: c.index, ID: "default/shop", Status: StackStarting, Host: &host})
	c.mustApply(KindSetCopySynced, HostActor(0), SetCopySynced{Volume: "default/data", DataGeneration: 1, Synced: true})
	return c
}

// withCopies is default/data with the given copies.
func (c *cluster) withCopies(copies ...VolumeCopy) Volume {
	volume := *c.state.Volumes["default/data"]
	volume.Copies = copies
	return volume
}

func TestOnlyAWorkerTakesAStackOrACopyWithADiskButAnyHostATiebreaker(t *testing.T) {
	c := moveCluster(t)
	c.state.Hosts[2].Roles = hostrole.Set{hostrole.ControlPlane, hostrole.Ingress}
	const notWorker = "host node2 has the roles control-plane,ingress, not worker"
	c.refuse(KindPutVolume, LeaderActor, PutVolume{PlannedAt: c.index, Volume: c.withCopies(
		VolumeCopy{Host: 0, Class: "fast"}, VolumeCopy{Host: 2, Class: "fast"},
	)}, notWorker)
	two := HostID(2)
	c.refuse(KindSetStackStatus, LeaderActor,
		SetStackStatus{PlannedAt: c.index, ID: "default/shop", Status: StackStarting, Host: &two}, notWorker)
	c.refuse(KindRequestMove, cliActor, RequestMove{Namespace: "default", Name: "shop", To: 2}, "host 2 is no active worker")
	zero := HostID(0)
	c.refuse(KindStartOperation, LeaderActor, StartOperation{
		Kind: OperationMoveStack, Locks: []string{"stack/default/shop"},
		Move: &StackMove{Stack: "default/shop", From: &zero, To: 2},
	}, "it is no active worker")

	c.mustApply(KindPutVolume, LeaderActor, PutVolume{PlannedAt: c.index, Volume: c.withCopies(
		VolumeCopy{Host: 0, Class: "fast"}, VolumeCopy{Host: 1, Class: "fast"}, VolumeCopy{Host: 2, Tiebreaker: true},
	)})
}

// startMove starts the leader's move of default/shop from host 0 to host
// 1 and returns its ID.
func (c *cluster) startMove() uint64 {
	c.t.Helper()
	from := HostID(0)
	c.mustApply(KindStartOperation, LeaderActor, StartOperation{
		Kind: OperationMoveStack, Locks: []string{"stack/default/shop", "volume/default/data"},
		Deadline: testTime.Add(10 * time.Minute), Move: &StackMove{Stack: "default/shop", From: &from, To: 1},
	})
	return c.index
}

func TestAMoveGoesFromTheStacksHostToAnActiveHost(t *testing.T) {
	c := moveCluster(t)
	zero, one := HostID(0), HostID(1)
	c.mustApply(KindSetHostPhase, LeaderActor, SetHostPhase{Host: 2, Phase: Draining})
	tests := []struct {
		name      string
		start     StartOperation
		wantError string
	}{
		{
			"without a move",
			StartOperation{Kind: OperationMoveStack, Locks: []string{"stack/default/shop"}},
			"a move goes with move-stack",
		},
		{"a move of another kind", StartOperation{
			Kind: "fence", Locks: []string{"host/1"},
			Move: &StackMove{Stack: "default/shop", From: &zero, To: 1},
		}, "a move goes with move-stack"},
		{"without the stack's lock", StartOperation{
			Kind: OperationMoveStack, Locks: []string{"volume/default/data"},
			Move: &StackMove{Stack: "default/shop", From: &zero, To: 1},
		}, "must lock it"},
		{"from another host", StartOperation{
			Kind: OperationMoveStack, Locks: []string{"stack/default/shop"},
			Move: &StackMove{Stack: "default/shop", From: &one, To: 2},
		}, "not where the move starts"},
		{"to its own host", StartOperation{
			Kind: OperationMoveStack, Locks: []string{"stack/default/shop"},
			Move: &StackMove{Stack: "default/shop", From: &zero, To: 0},
		}, "on host 0 already"},
		{"to a draining host", StartOperation{
			Kind: OperationMoveStack, Locks: []string{"stack/default/shop"},
			Move: &StackMove{Stack: "default/shop", From: &zero, To: 2},
		}, "it is no active worker"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c.refuse(KindStartOperation, LeaderActor, test.start, test.wantError)
		})
	}
	c.startMove()
}

func TestAMoveStopsTheStackOnceTheCopiesAreSyncedAndNotWhenItRollsBack(t *testing.T) {
	c := moveCluster(t)
	operation := c.startMove()
	one := HostID(1)
	c.mustApply(KindAdvanceOperation, LeaderActor, AdvanceOperation{ID: operation, FromStep: 0, Step: MoveCopies, Owner: &one})
	if c.state.StopsForMove("default/shop") {
		t.Error("the stack stops before its copies are synced")
	}
	zero := HostID(0)
	c.mustApply(KindAdvanceOperation, HostActor(1), AdvanceOperation{ID: operation, FromStep: MoveCopies, Step: MoveSynced, Owner: &zero})
	if !c.state.StopsForMove("default/shop") {
		t.Error("the stack runs on once its copies are synced")
	}
	c.mustApply(KindRollBackOperation, LeaderActor, RollBackOperation{ID: operation, FromStep: MoveSynced, Error: "late"})
	if c.state.StopsForMove("default/shop") {
		t.Error("a move that rolls back still stops the stack")
	}
	c.refuse(KindAdvanceOperation, HostActor(0), AdvanceOperation{ID: operation, FromStep: MoveSynced, Step: MoveStopped},
		"rolls back")
	c.mustApply(KindFinishOperation, LeaderActor, FinishOperation{ID: operation})
	if got := c.state.Operations[operation]; got.State != OpFailed || got.Error != "late" {
		t.Errorf("a rolled back operation fails with why: %+v", got)
	}
	if c.state.Stacks["default/shop"].MoveFailed.IsZero() {
		t.Error("the stack does not know its move failed")
	}
}

func TestAMoveRollsBackOnlyBeforeItsPointOfNoReturn(t *testing.T) {
	c := moveCluster(t)
	operation := c.startMove()
	c.refuse(KindRollBackOperation, HostActor(1), RollBackOperation{ID: operation, Error: "x"}, "only the leader does")
	c.refuse(KindRollBackOperation, LeaderActor, RollBackOperation{ID: operation}, "a rollback says why")
	c.refuse(KindRollBackOperation, LeaderActor, RollBackOperation{ID: operation, FromStep: 2, Error: "x"},
		"at step 0, no longer 2")
	c.mustApply(KindAdvanceOperation, LeaderActor, AdvanceOperation{ID: operation, FromStep: 0, Step: MoveStopped})
	c.refuse(KindRollBackOperation, LeaderActor, RollBackOperation{ID: operation, FromStep: MoveStopped, Error: "x"},
		"past its point of no return")
}

func TestAnExtensionGivesTheCurrentStepMoreTime(t *testing.T) {
	c := moveCluster(t)
	operation := c.startMove()
	deadline := c.state.Operations[operation].Deadline
	c.refuse(KindExtendOperation, HostActor(1), ExtendOperation{ID: operation, Deadline: deadline.Add(time.Minute)},
		"only the leader does")
	c.refuse(KindExtendOperation, LeaderActor, ExtendOperation{ID: operation, Deadline: deadline}, "not later than")
	c.refuse(KindExtendOperation, LeaderActor, ExtendOperation{ID: operation, Step: 1, Deadline: deadline.Add(time.Minute)},
		"at step 0, no longer 1")
	c.mustApply(KindExtendOperation, LeaderActor, ExtendOperation{ID: operation, Deadline: deadline.Add(time.Minute)})
	if got := c.state.Operations[operation].Deadline; !got.Equal(deadline.Add(time.Minute)) {
		t.Errorf("deadline %s", got)
	}
}

func TestAUserAsksToMoveAStackAndTheMovesEndAnswersIt(t *testing.T) {
	c := moveCluster(t)
	c.mustApply(KindSetHostPhase, LeaderActor, SetHostPhase{Host: 2, Phase: Draining})
	c.refuse(KindRequestMove, cliActor, RequestMove{Namespace: "default", Name: "shop", To: 0}, "runs on host 0 already")
	c.refuse(KindRequestMove, cliActor, RequestMove{Namespace: "default", Name: "shop", To: 2}, "no active worker")
	c.refuse(KindRequestMove, cliActor, RequestMove{Namespace: "default", Name: "blog", To: 1}, "no stack default/blog")
	c.mustApply(KindRequestMove, cliActor, RequestMove{Namespace: "default", Name: "shop", To: 1})
	if got := c.state.Stacks["default/shop"].MoveTo; got == nil || *got != 1 {
		t.Fatalf("move to %v", got)
	}

	operation := c.startMove()
	c.mustApply(KindAdvanceOperation, LeaderActor, AdvanceOperation{ID: operation, FromStep: 0, Step: MoveStopped})
	one := HostID(1)
	c.mustApply(KindPutVolume, LeaderActor, PutVolume{PlannedAt: c.index, Operation: operation, Volume: c.withCopies(
		VolumeCopy{Host: 0, Class: "fast"}, VolumeCopy{Host: 1, Class: "fast"},
	)})
	c.mustApply(KindSetStackStatus, LeaderActor, SetStackStatus{
		PlannedAt: c.index, ID: "default/shop", Status: StackStarting, Host: &one, Operation: operation,
	})
	if got := c.state.Stacks["default/shop"].LastMove; !got.Equal(testTime) {
		t.Errorf("last move %s, want %s", got, testTime)
	}
	c.mustApply(KindFinishOperation, LeaderActor, FinishOperation{ID: operation})
	if got := c.state.Stacks["default/shop"]; got.MoveTo != nil || *got.Host != 1 {
		t.Errorf("after the move: host %v, move to %v", *got.Host, got.MoveTo)
	}
}

func TestAHostWithStacksLeavesInsteadOfGoingAtOnce(t *testing.T) {
	c := moveCluster(t)
	c.activeHost(3)
	if err := c.state.CheckRemoval(0); !errors.Is(err, ErrNotEmpty) {
		t.Fatalf("a host with a stack: %v", err)
	}
	if err := c.state.CheckLeaving(0); err != nil {
		t.Fatalf("it may leave: %v", err)
	}
	c.refuse(KindSetHostPhase, cliActor, SetHostPhase{Host: 0, Phase: Active, Leaving: true}, "only a draining host leaves")
	c.mustApply(KindSetHostPhase, cliActor, SetHostPhase{Host: 0, Phase: Draining, Leaving: true, From: Active})
	if host := c.state.Hosts[0]; host.Phase != Draining || !host.Leaving {
		t.Fatalf("host 0 is %s, leaving %t", host.Phase, host.Leaving)
	}
	// once draining, it may still leave; an undrain takes it back
	c.mustApply(KindSetHostPhase, cliActor, SetHostPhase{Host: 0, Phase: Active})
	c.mustApply(KindSetHostPhase, cliActor, SetHostPhase{Host: 0, Phase: Draining})
	c.mustApply(KindSetHostPhase, cliActor, SetHostPhase{Host: 0, Phase: Draining, Leaving: true})
	c.mustApply(KindSetHostPhase, cliActor, SetHostPhase{Host: 0, Phase: Active})
	if c.state.Hosts[0].Leaving {
		t.Error("an undrain keeps the host leaving")
	}
	c.mustApply(KindSetHostPhase, cliActor, SetHostPhase{Host: 0, Phase: Draining, Leaving: true})
	// a highly available cluster keeps three hosts that do not leave
	c.refuse(KindSetHostPhase, cliActor, SetHostPhase{Host: 1, Phase: Draining, Leaving: true},
		"join a replacement first")
}

func TestARequestToMoveIsTakenBackOrDroppedWithItsHost(t *testing.T) {
	c := moveCluster(t)
	request := func(to HostID) {
		c.t.Helper()
		c.mustApply(KindRequestMove, cliActor, RequestMove{Namespace: "default", Name: "shop", To: to})
	}
	request(1)
	request(0) // where it runs: the request goes
	if got := c.state.Stacks["default/shop"].MoveTo; got != nil {
		t.Errorf("a move to its own host kept the request to host %d", *got)
	}
	request(1)
	c.mustApply(KindSetHostPhase, LeaderActor, SetHostPhase{Host: 1, Phase: Draining})
	if got := c.state.Stacks["default/shop"].MoveTo; got != nil {
		t.Errorf("a request to a draining host stays: host %d", *got)
	}
}

func TestTheNewHostRunsTheStackOnceItHasIt(t *testing.T) {
	c := moveCluster(t)
	operation := c.startMove()
	c.mustApply(KindAdvanceOperation, LeaderActor, AdvanceOperation{ID: operation, FromStep: 0, Step: MoveStopped})
	if !c.state.StopsForMove("default/shop") {
		t.Fatal("the old host runs the stack after it stopped it")
	}
	one := HostID(1)
	c.mustApply(KindSetStackStatus, LeaderActor, SetStackStatus{
		PlannedAt: c.index, ID: "default/shop", Status: StackStarting, Host: &one, Operation: operation,
	})
	c.mustApply(KindAdvanceOperation, LeaderActor, AdvanceOperation{ID: operation, FromStep: MoveStopped, Step: MoveHost})
	if c.state.StopsForMove("default/shop") {
		t.Error("the new host does not run the stack it has")
	}
}
