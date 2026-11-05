package controller

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/webcodingcat/clowder/internal/resource"
	"github.com/webcodingcat/clowder/internal/stackspec"
	"github.com/webcodingcat/clowder/internal/state"
)

// addActiveHost joins host id, active, with host 0's capacity and pools.
func addActiveHost(l *leader, id state.HostID) {
	l.t.Helper()
	token := "token-" + strconv.Itoa(int(id))
	l.apply(state.KindAddJoinToken, state.UserActor("alice"), state.AddJoinToken{
		ID: token, Hash: strings.Repeat("ab", 32), TTL: time.Hour,
	})
	l.apply(state.KindAddHost, state.LeaderActor, state.AddHost{Token: token, Host: state.Host{
		ID: id, Name: "node" + strconv.Itoa(int(id)), Underlay: underlayOf(id), PublicAddress: publicAddressOf(id),
		WGPublicKey: "wg", CertificateSerial: "s" + strconv.Itoa(int(id)), CertificateNotAfter: time.Now().AddDate(1, 0, 0),
		ServerID: 40 + int64(id), ServerChecked: true,
	}})
	l.apply(state.KindSetHostPhase, state.LeaderActor, state.SetHostPhase{Host: id, Phase: state.Active})
	var report state.ReportHost
	l.Read(func(current *state.State) {
		first := current.Hosts[0]
		report = state.ReportHost{Host: id, Version: "1.0.0", Capacity: first.Capacity, Pools: first.Pools}
	})
	l.apply(state.KindReportHost, state.HostActor(id), report)
}

// newMover is the move loop of l, for passes one by one; progress answers
// for the hosts.
func newMover(l *leader, progress Progress) *mover {
	return &mover{
		cluster: l, propose: l.propose, progress: progress, logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		looks: map[uint64]look{},
	}
}

// runningShopOnZero places shop on host 0 and has the host report it
// running, its copy initialized and synced.
func runningShopOnZero(t *testing.T, l *leader) {
	t.Helper()
	newPlacer(l).pass(context.Background())
	zero := state.HostID(0)
	l.apply(state.KindSetVolumeInitialized, state.HostActor(0), state.SetVolumeInitialized{Volume: "default/db", DataGeneration: 1})
	l.apply(state.KindSetCopySynced, state.HostActor(0), state.SetCopySynced{Volume: "default/db", DataGeneration: 1, Synced: true})
	l.apply(state.KindSetStackStatus, state.HostActor(0), state.SetStackStatus{
		ID: "default/shop", Status: state.StackRunning, Host: &zero, Generation: 1,
	})
}

// move is the move that holds shop's lock.
func (l *leader) move() *state.Operation {
	var operation *state.Operation
	l.Read(func(current *state.State) { operation = current.MoveOf("default/shop") })
	return operation
}

func TestAStackMovesWhereAUserAsksStepByStep(t *testing.T) {
	l := newLeader(t, 100<<30)
	addActiveHost(l, 1)
	runningShopOnZero(t, l)
	l.apply(state.KindRequestMove, state.UserActor("alice"), state.RequestMove{Namespace: "default", Name: "shop", To: 1})
	placer, mover := newPlacer(l), newMover(l, nil)
	ctx := context.Background()

	placer.pass(ctx)
	operation := l.move()
	if operation == nil || operation.Step != 0 {
		t.Fatalf("the placement started no move: %+v", operation)
	}
	mover.pass(ctx)
	operation = l.move()
	var copies []state.VolumeCopy
	l.Read(func(current *state.State) { copies = current.Volumes["default/db"].Copies })
	if operation.Step != state.MoveCopies || *operation.Owner != 1 || len(copies) != 2 {
		t.Fatalf("after the first step: %+v, copies %+v", operation, copies)
	}

	// the hosts run their steps: host 1 syncs, host 0 stops the stack
	one, zero := state.HostID(1), state.HostID(0)
	l.apply(state.KindSetCopySynced, state.HostActor(1), state.SetCopySynced{Volume: "default/db", DataGeneration: 1, Synced: true})
	l.apply(state.KindAdvanceOperation, state.HostActor(1), state.AdvanceOperation{
		ID: operation.ID, FromStep: state.MoveCopies, Step: state.MoveSynced, Owner: &zero,
	})
	l.apply(state.KindAdvanceOperation, state.HostActor(0), state.AdvanceOperation{
		ID: operation.ID, FromStep: state.MoveSynced, Step: state.MoveStopped,
	})
	mover.pass(ctx)
	var stack state.Stack
	l.Read(func(current *state.State) { stack = *current.Stacks["default/shop"] })
	if *stack.Host != 1 || stack.Status != state.StackStarting || l.move().Step != state.MoveHost {
		t.Fatalf("after the old host stopped: host %d, %s, %+v", *stack.Host, stack.Status, l.move())
	}
	l.apply(state.KindSetStackStatus, state.HostActor(1), state.SetStackStatus{
		ID: "default/shop", Status: state.StackRunning, Host: &one, Generation: 1,
	})
	mover.pass(ctx)
	if l.move() != nil {
		t.Fatalf("the move did not end: %+v", l.move())
	}

	// the copy left behind on host 0 goes
	placer.pass(ctx)
	l.Read(func(current *state.State) { copies = current.Volumes["default/db"].Copies })
	if len(copies) != 1 || copies[0].Host != 1 {
		t.Errorf("copies after the move: %+v", copies)
	}
	l.Read(func(current *state.State) { stack = *current.Stacks["default/shop"] })
	if stack.MoveTo != nil || stack.LastMove.IsZero() {
		t.Errorf("after the move: move to %v, last move %s", stack.MoveTo, stack.LastMove)
	}
}

func TestALateResyncGetsMoreTimeOnlyWhileItGetsOn(t *testing.T) {
	l := newLeader(t, 100<<30)
	addActiveHost(l, 1)
	runningShopOnZero(t, l)
	l.apply(state.KindRequestMove, state.UserActor("alice"), state.RequestMove{Namespace: "default", Name: "shop", To: 1})
	left := map[string]int64{"default/db": 3 << 30}
	progress := func(_ context.Context, host state.HostID) (map[string]int64, error) {
		if host != 1 {
			t.Errorf("asked host %d", host)
		}
		return left, nil
	}
	placer, mover := newPlacer(l), newMover(l, progress)
	ctx := context.Background()
	placer.pass(ctx)
	mover.pass(ctx)

	late := func() {
		t.Helper()
		var copied *state.State
		l.Read(func(current *state.State) { copied = current })
		operation := copied.MoveOf("default/shop")
		if err := mover.step(ctx, copied, operation, operation.Deadline.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	// the first look has nothing to compare with, the second sees progress
	late()
	deadline := l.move().Deadline
	left["default/db"] = 2 << 30
	late()
	if got := l.move(); got.State != state.OpRunning || !got.Deadline.After(deadline) {
		t.Fatalf("a resync that gets on: %+v", got)
	}
	// no progress: it rolls back, and ends
	late()
	if got := l.move(); got.State != state.OpRollingBack {
		t.Fatalf("a resync that does not get on: %+v", got)
	}
	mover.pass(ctx)
	if l.move() != nil {
		t.Fatal("a rolled back move did not end")
	}
	var stack state.Stack
	l.Read(func(current *state.State) { stack = *current.Stacks["default/shop"] })
	if *stack.Host != 0 || stack.MoveTo != nil {
		t.Errorf("after the rollback: host %d, move to %v", *stack.Host, stack.MoveTo)
	}
	// the copy the move left goes with the next placement
	placer.pass(ctx)
	var copies []state.VolumeCopy
	l.Read(func(current *state.State) { copies = current.Volumes["default/db"].Copies })
	if len(copies) != 1 || copies[0].Host != 0 {
		t.Errorf("copies after the rollback: %+v", copies)
	}
}

// startedMove has the placement start shop's move to host 1 and returns
// the mover, whose progress answers with left, or err.
func startedMove(t *testing.T, left map[string]int64, err *error) (*leader, *mover) {
	t.Helper()
	l := newLeader(t, 100<<30)
	addActiveHost(l, 1)
	runningShopOnZero(t, l)
	l.apply(state.KindRequestMove, state.UserActor("alice"), state.RequestMove{Namespace: "default", Name: "shop", To: 1})
	progress := func(context.Context, state.HostID) (map[string]int64, error) { return left, *err }
	placer, mover := newPlacer(l), newMover(l, progress)
	placer.pass(context.Background())
	return l, mover
}

// lateStep runs the mover's step as if the move's deadline had passed.
func (m *mover) lateStep(t *testing.T, l *leader) {
	t.Helper()
	var copied *state.State
	l.Read(func(current *state.State) { copied = current })
	operation := copied.MoveOf("default/shop")
	if err := m.step(context.Background(), copied, operation, operation.Deadline.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
}

func TestALateResyncSurvivesOneUnansweredLookAndWaitsOnceForTheImages(t *testing.T) {
	left, err := map[string]int64{"default/db": 1 << 30}, error(nil)
	l, mover := startedMove(t, left, &err)
	mover.pass(context.Background())
	err = errors.New("connection refused")
	mover.lateStep(t, l) // no answer at the first look: a window
	if got := l.move(); got.State != state.OpRunning {
		t.Fatalf("an unanswered first look rolled back: %+v", got)
	}
	err = nil
	mover.lateStep(t, l) // an answer after none: a window
	l.apply(state.KindSetCopySynced, state.HostActor(1), state.SetCopySynced{Volume: "default/db", DataGeneration: 1, Synced: true})
	mover.lateStep(t, l) // synced, the images still coming: a window
	if got := l.move(); got.State != state.OpRunning {
		t.Fatalf("a synced resync rolled back: %+v", got)
	}
	mover.lateStep(t, l)
	if got := l.move(); got.State != state.OpRollingBack || !strings.Contains(got.Error, "images did not come") {
		t.Errorf("the images did not come: %+v", got)
	}
}

func TestAMoveWhoseCopiesCannotGoToTheNewHostRollsBack(t *testing.T) {
	err := error(nil)
	l, mover := startedMove(t, nil, &err)
	// the new host's pool is gone meanwhile
	l.apply(state.KindReportHost, state.HostActor(1), state.ReportHost{
		Host: 1, Version: "1.0.0", Capacity: state.Resources{MemoryBytes: 4 << 30, CPUs: 4},
	})
	mover.pass(context.Background())
	if got := l.move(); got.State != state.OpRollingBack || !strings.Contains(got.Error, "fits no pool") {
		t.Errorf("a move without room for its copies: %+v", got)
	}
}

func TestAMoveLatePastItsPointOfNoReturnEndsAsFailedWhereItGot(t *testing.T) {
	err := error(nil)
	l, mover := startedMove(t, nil, &err)
	operation := l.move()
	l.apply(state.KindAdvanceOperation, state.LeaderActor, state.AdvanceOperation{
		ID: operation.ID, FromStep: state.MoveStart, Step: state.MoveStopped,
	})
	mover.pass(context.Background()) // the leader gives the stack its new host
	mover.lateStep(t, l)             // and host 1 never reports it running
	var got *state.Operation
	l.Read(func(current *state.State) { got = current.Operations[operation.ID] })
	if got.State != state.OpFailed || !strings.Contains(got.Error, "giving the stack its new host") {
		t.Errorf("a move late after its point of no return: %+v", got)
	}
	var stack state.Stack
	l.Read(func(current *state.State) { stack = *current.Stacks["default/shop"] })
	if *stack.Host != 1 {
		t.Errorf("the stack went back to host %d", *stack.Host)
	}
}

func TestTheResyncTimeOfALargeVolumeDoesNotOverflow(t *testing.T) {
	l := newLeader(t, 1<<40)
	putResource(l, resource.KindVolume, "big", resource.VolumeSpec{Class: "fast", Size: 100 << 30, Replicas: 2})
	putResource(l, resource.KindStack, "archive", stackspec.Spec{Services: map[string]stackspec.Service{
		"app": {Image: "x", Volumes: []stackspec.Mount{{Volume: "big", Path: "/big"}}},
	}})
	var copied *state.State
	l.Read(func(current *state.State) { copied = current })
	// 100 GiB at the default 50M (MiB) a second, twice over: about 68 minutes
	got := syncTime(copied, "default/archive")
	if got < state.StepTime+65*time.Minute || got > state.StepTime+70*time.Minute {
		t.Errorf("sync time of 100 GiB: %s", got)
	}
}

func TestAMoveWhoseHostFailsEndsAtOnce(t *testing.T) {
	tests := []struct {
		name      string
		step      int
		failed    state.HostID
		wantState state.OpState
	}{
		{"the new host before the stack stopped: a rollback", state.MoveStart, 1, state.OpRollingBack},
		{"the old host before the stack stopped: a rollback", state.MoveStart, 0, state.OpRollingBack},
		{"the new host after the stack stopped: the move fails", state.MoveStopped, 1, state.OpFailed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := error(nil)
			l, mover := startedMove(t, nil, &err)
			operation := l.move()
			if test.step != state.MoveStart {
				l.apply(state.KindAdvanceOperation, state.LeaderActor, state.AdvanceOperation{
					ID: operation.ID, FromStep: state.MoveStart, Step: test.step,
				})
			}
			l.apply(state.KindSetHostPhase, state.LeaderActor, state.SetHostPhase{Host: test.failed, Phase: state.Failed})
			mover.pass(context.Background())
			var got *state.Operation
			l.Read(func(current *state.State) { got = current.Operations[operation.ID] })
			if got.State != test.wantState || !strings.Contains(got.Error, "is failed") {
				t.Errorf("operation %+v", got)
			}
			var stack state.Stack
			l.Read(func(current *state.State) { stack = *current.Stacks["default/shop"] })
			if *stack.Host != 0 {
				t.Errorf("the stack is on host %d", *stack.Host)
			}
		})
	}
}
