package controller

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/webcodingcat/clowder/internal/hetzner/hetznertest"
	"github.com/webcodingcat/clowder/internal/hostrole"
	"github.com/webcodingcat/clowder/internal/resource"
	"github.com/webcodingcat/clowder/internal/stackspec"
	"github.com/webcodingcat/clowder/internal/state"
)

// testFencer is the fencing loop of host 0 in a cluster of three voters;
// the hosts have the checked servers 40 to 42 at api. silences are what
// the other voters say when asked: voter -> host -> silence; a voter that
// is missing does not answer.
type testFencer struct {
	*Fencing
	l        *leader
	api      *hetznertest.API
	clock    time.Time
	silences map[state.HostID]map[state.HostID]time.Duration
}

func newTestFencer(t *testing.T) *testFencer {
	t.Helper()
	l := newLeader(t, 100<<30)
	withServers(l)
	api := withHetzner(t, l, "t")
	l.mu.Lock()
	for _, host := range l.state.Hosts {
		host.Voter, host.ServerChecked = true, true
	}
	// host 1 runs shop: its failure needs a fence
	one := state.HostID(1)
	l.state.Stacks["default/shop"].Host = &one
	l.mu.Unlock()
	test := &testFencer{l: l, api: api, clock: time.Now(), silences: map[state.HostID]map[state.HostID]time.Duration{}}
	heard := NewHeard()
	heard.now = func() time.Time { return test.clock }
	heard.started = test.clock
	ask := func(_ context.Context, voter, host state.HostID) (time.Duration, error) {
		silence, ok := test.silences[voter][host]
		if !ok {
			return 0, errors.New("no answer")
		}
		return silence, nil
	}
	test.Fencing = NewFencing(FenceParts{
		Cluster: l, Propose: l.propose, Heard: heard, Ask: ask, Self: 0, Conditions: NewConditions(),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Open:   openPadded,
	})
	test.pause = time.Millisecond
	return test
}

// pass runs one pass of the loop, which must not fail, and waits for the
// power-offs it started.
func (f *testFencer) pass(t *testing.T) {
	t.Helper()
	f.passWithin(t)
	f.jobs.Wait()
}

// passWithin runs one pass of the loop, which must not fail and must not
// wait for a power-off.
func (f *testFencer) passWithin(t *testing.T) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- f.Fencing.pass(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the pass waits for a power-off")
	}
}

func (f *testFencer) phase(id state.HostID) state.HostPhase {
	var phase state.HostPhase
	f.l.Read(func(current *state.State) { phase = current.Hosts[id].Phase })
	return phase
}

// silent lets time pass in which the leader hears every host but host 1.
func (f *testFencer) silent(d time.Duration) {
	f.clock = f.clock.Add(d)
	for _, id := range []state.HostID{0, 2} {
		f.Heard.Beat(id)
	}
}

func TestAHostIsFailedOnlyWhenAMajorityOfTheVotersMissesIt(t *testing.T) {
	tests := []struct {
		name     string
		voter2   map[state.HostID]time.Duration // what voter 2 says, nil: no answer
		wantFail bool
	}{
		{"the other voter misses it too", map[state.HostID]time.Duration{1: FailAfter}, true},
		{"the other voter hears it: only the leader is cut off", map[state.HostID]time.Duration{1: time.Second}, false},
		{"the other voter does not answer", nil, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newTestFencer(t)
			f.Heard.Beat(1)
			f.silent(FailAfter - time.Second)
			f.silences[2] = map[state.HostID]time.Duration{1: FailAfter}
			f.pass(t)
			if phase := f.phase(1); phase != state.Active {
				t.Fatalf("failed before the leader missed it for %v: %s", FailAfter, phase)
			}
			f.silent(time.Second)
			if test.voter2 != nil {
				f.silences[2] = test.voter2
			} else {
				delete(f.silences, 2)
			}
			f.pass(t)
			if failed := f.phase(1) != state.Active; failed != test.wantFail {
				t.Errorf("failed %v, want %v", failed, test.wantFail)
			}
		})
	}
}

func TestAFailedHostThatAMajorityHearsAgainIsActive(t *testing.T) {
	f := newTestFencer(t)
	f.l.mu.Lock()
	f.l.state.Hosts[1].Phase = state.Failed
	f.l.state.Hosts[1].ServerChecked = false // so it is not fenced meanwhile
	f.l.mu.Unlock()
	f.Heard.Beat(1)
	f.silences[2] = map[state.HostID]time.Duration{1: FailAfter}
	f.pass(t)
	if phase := f.phase(1); phase != state.Failed {
		t.Fatalf("back while the other voter misses it: %s", phase)
	}
	f.silences[2] = map[state.HostID]time.Duration{1: time.Second}
	f.pass(t)
	if phase := f.phase(1); phase != state.Active {
		t.Errorf("phase %s, want active", phase)
	}
}

func TestAFailedHostIsPoweredOffFencedAndPoweredOnAgain(t *testing.T) {
	f := newTestFencer(t)
	f.l.apply(state.KindSetHostPhase, state.LeaderActor, state.SetHostPhase{Host: 1, Phase: state.Failed})
	f.pass(t) // starts the fence
	if phase := f.phase(1); phase != state.Fencing {
		t.Fatalf("phase %s, want fencing", phase)
	}
	f.pass(t) // powers off, fences
	if phase := f.phase(1); phase != state.Fenced {
		t.Fatalf("phase %s, want fenced", phase)
	}
	f.l.mu.Lock()
	two := state.HostID(2)
	f.l.state.Stacks["default/shop"].Host, f.l.state.Stacks["default/shop"].Status = &two, state.StackRunning
	f.l.mu.Unlock()
	f.pass(t) // shop runs elsewhere: the fence is over
	f.pass(t) // powers on
	if want := []string{"poweroff 41", "poweron 41"}; !slices.Equal(f.api.Changes(), want) {
		t.Errorf("power actions %v, want %v", f.api.Changes(), want)
	}
	f.pass(t) // once
	if len(f.api.Changes()) != 2 {
		t.Errorf("power actions %v", f.api.Changes())
	}
	f.l.Read(func(current *state.State) {
		if current.HostLocked(1) {
			t.Error("the fence still holds the host")
		}
	})
}

func TestAHostThatCannotBePoweredOffStaysFailed(t *testing.T) {
	f := newTestFencer(t)
	f.api.FailActions(41, 2*powerOffTries)
	f.l.apply(state.KindSetHostPhase, state.LeaderActor, state.SetHostPhase{Host: 1, Phase: state.Failed})
	f.pass(t)
	f.pass(t)
	if phase := f.phase(1); phase != state.Failed {
		t.Errorf("phase %s, want failed", phase)
	}
	if len(f.api.Changes()) != powerOffTries {
		t.Errorf("power actions %v", f.api.Changes())
	}
	f.pass(t) // no new fence right away
	if phase := f.phase(1); phase != state.Failed || len(f.api.Changes()) != powerOffTries {
		t.Errorf("phase %s, power actions %v", phase, f.api.Changes())
	}
	if _, blocked := f.Conditions.Since(fencingBlockedKey(1)); !blocked {
		t.Error("no condition for the alert")
	}
}

// TestAHangingPowerOffHoldsUpNoOtherHost hangs the power-off of worker 1.
// Meanwhile worker 2 fails, is fenced and powered off. Host 0 is the only
// voter and a worker too.
func TestAHangingPowerOffHoldsUpNoOtherHost(t *testing.T) {
	f := newTestFencer(t)
	putResource(f.l, resource.KindStack, "blog", stackspec.Spec{Services: map[string]stackspec.Service{
		"app": {Image: "x"},
	}})
	f.l.mu.Lock()
	for id := state.HostID(1); id <= 2; id++ {
		f.l.state.Hosts[id].Roles, f.l.state.Hosts[id].Voter = hostrole.Set{hostrole.Worker}, false
	}
	two := state.HostID(2)
	f.l.state.Stacks["default/blog"].Host = &two
	f.l.mu.Unlock()
	release := f.api.HoldActions(41)
	defer release()

	f.silent(FailAfter)
	f.passWithin(t) // fails host 1
	f.passWithin(t) // starts its fence
	f.passWithin(t) // and its power-off, which hangs
	waitFor(t, f.l, "the power-off of host 1", func(*state.State) bool {
		return slices.Contains(f.api.Changes(), "poweroff 41")
	})

	f.clock = f.clock.Add(FailAfter) // the leader hears only itself
	f.Heard.Beat(0)
	f.passWithin(t) // fails host 2
	if phase := f.phase(2); phase != state.Failed {
		t.Fatalf("host 2 is %s while host 1's power-off hangs, want failed", phase)
	}
	f.passWithin(t) // starts the fence of host 2
	f.passWithin(t) // and its power-off
	waitFor(t, f.l, "host 2 fenced", func(current *state.State) bool {
		return current.Hosts[2].Phase == state.Fenced
	})
	if phase := f.phase(1); phase != state.Fencing {
		t.Errorf("host 1 is %s, want fencing while its power-off hangs", phase)
	}

	release()
	f.jobs.Wait()
	if phase := f.phase(1); phase != state.Fenced {
		t.Errorf("host 1 is %s after its power-off, want fenced", phase)
	}
	if want := []string{"poweroff 41", "poweroff 42"}; !slices.Equal(f.api.Changes(), want) {
		t.Errorf("power actions %v, want %v", f.api.Changes(), want)
	}
}

func TestNoHostIsFencedWhenSeveralFailedAtOnce(t *testing.T) {
	f := newTestFencer(t)
	for id := state.HostID(1); id <= 2; id++ {
		f.l.apply(state.KindSetHostPhase, state.LeaderActor, state.SetHostPhase{Host: id, Phase: state.Failed})
	}
	f.pass(t)
	for id := state.HostID(1); id <= 2; id++ {
		if phase := f.phase(id); phase != state.Failed {
			t.Errorf("host %d is %s", id, phase)
		}
	}
	if _, blocked := f.Conditions.Since(fencingBlockedKey(1)); !blocked {
		t.Error("no condition for the alert")
	}
	if len(f.api.Changes()) != 0 {
		t.Errorf("power actions %v", f.api.Changes())
	}
}

func TestAHostFencedTooOftenStaysOff(t *testing.T) {
	f := newTestFencer(t)
	f.l.mu.Lock()
	now := time.Now()
	f.l.state.Hosts[1].Phase = state.Fenced
	f.l.state.Hosts[1].FencedAt = []time.Time{now.Add(-50 * time.Minute), now.Add(-20 * time.Minute), now}
	f.l.mu.Unlock()
	f.pass(t)
	if phase := f.phase(1); phase != state.Disabled {
		t.Errorf("phase %s, want disabled", phase)
	}
	if len(f.api.Changes()) != 0 {
		t.Errorf("power actions %v", f.api.Changes())
	}
}

func TestTheLeaderAsksNoVoterAboutItself(t *testing.T) {
	f := newTestFencer(t)
	f.silent(FailAfter)
	// host 1 would say it misses itself, but is not asked
	f.silences[1] = map[state.HostID]time.Duration{1: FailAfter}
	var copied *state.State
	f.l.Read(func(current *state.State) { copied = current })
	if f.voters(context.Background(), copied, 1, func(silence time.Duration) bool { return silence >= FailAfter }) {
		t.Error("a majority without the other voter")
	}
}

func TestAHostWithoutAStackToTakeOverIsNotFenced(t *testing.T) {
	f := newTestFencer(t)
	f.l.mu.Lock()
	f.l.state.Stacks["default/shop"].Host = nil // drained for maintenance
	f.l.mu.Unlock()
	f.l.apply(state.KindSetHostPhase, state.LeaderActor, state.SetHostPhase{Host: 1, Phase: state.Failed})
	f.pass(t)
	if phase := f.phase(1); phase != state.Failed {
		t.Errorf("phase %s, want failed", phase)
	}
	if _, blocked := f.Conditions.Since(fencingBlockedKey(1)); blocked {
		t.Error("a host that needs no fence is blocked")
	}
	// nor one whose stacks no other host could take
	f.l.mu.Lock()
	one := state.HostID(1)
	f.l.state.Stacks["default/shop"].Host = &one
	f.l.state.Hosts[2].Phase = state.Draining
	f.l.state.Hosts[0].Phase = state.Draining
	f.l.mu.Unlock()
	f.pass(t)
	if phase := f.phase(1); phase != state.Failed || len(f.api.Changes()) != 0 {
		t.Errorf("phase %s, power actions %v", phase, f.api.Changes())
	}
	// nor one whose stacks only control-plane hosts without the worker
	// role could take
	f.l.mu.Lock()
	for _, id := range []state.HostID{0, 2} {
		f.l.state.Hosts[id].Phase, f.l.state.Hosts[id].Roles = state.Active, hostrole.Set{hostrole.ControlPlane}
	}
	f.l.mu.Unlock()
	f.pass(t)
	if phase := f.phase(1); phase != state.Failed || len(f.api.Changes()) != 0 {
		t.Errorf("phase %s, power actions %v", phase, f.api.Changes())
	}
}
