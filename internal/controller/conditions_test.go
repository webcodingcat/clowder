package controller

import (
	"sync"
	"testing"

	"github.com/webcodingcat/clowder/internal/state"
)

func TestAConditionBeginsOnceAndKeepsItsStart(t *testing.T) {
	conditions := NewConditions()
	if _, ok := conditions.Since("docker-down/0"); ok || conditions.Seen("docker-down/0") {
		t.Fatal("a new leader knows a condition")
	}
	if !conditions.Observe("docker-down/0", true) {
		t.Error("the first observation did not begin the condition")
	}
	since, _ := conditions.Since("docker-down/0")
	if conditions.Observe("docker-down/0", true) {
		t.Error("observing it again began the condition again")
	}
	if again, ok := conditions.Since("docker-down/0"); !ok || !again.Equal(since) {
		t.Errorf("since %v, %v after observing it again; want %v", again, ok, since)
	}

	if conditions.Observe("docker-down/0", false) {
		t.Error("an ended condition began")
	}
	if _, ok := conditions.Since("docker-down/0"); ok {
		t.Error("the condition holds after it ended")
	}
	if !conditions.Seen("docker-down/0") {
		t.Error("a condition looked at and ended is not seen")
	}
	if !conditions.Observe("docker-down/0", true) {
		t.Error("the condition did not begin anew after it ended")
	}
}

func TestAConditionThatNeverHeldIsSeen(t *testing.T) {
	conditions := NewConditions()
	if conditions.Observe(ingressNotHeld, false) {
		t.Error("a condition that does not hold began")
	}
	if _, ok := conditions.Since(ingressNotHeld); ok || !conditions.Seen(ingressNotHeld) {
		t.Errorf("held %v, seen %v; want not held, seen", ok, conditions.Seen(ingressNotHeld))
	}
}

func TestResetForgetsWhatTheLeaderSaw(t *testing.T) {
	conditions := NewConditions()
	conditions.Observe("docker-down/0", true)
	conditions.Observe(ingressNotHeld, false)
	conditions.Reset()
	for _, key := range []string{"docker-down/0", ingressNotHeld} {
		if _, ok := conditions.Since(key); ok || conditions.Seen(key) {
			t.Errorf("%s is known after a reset", key)
		}
	}
	if !conditions.Observe("docker-down/0", true) {
		t.Error("the condition did not begin anew after a reset")
	}
}

func TestConditionsTakeLoopsAtOnce(t *testing.T) {
	// the leader's loops observe conditions while the alerts read them; the
	// race detector checks the lock
	conditions := NewConditions()
	var loops sync.WaitGroup
	for host := range 4 {
		key := dockerDownKey(state.HostID(host))
		loops.Go(func() {
			for round := range 100 {
				conditions.Observe(key, round%2 == 0)
				conditions.Since(key)
				conditions.Seen(key)
			}
		})
	}
	loops.Go(func() {
		for range 10 {
			conditions.Reset()
		}
	})
	loops.Wait()
}
