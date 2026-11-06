package passes

import (
	"context"
	"testing"
	"time"
)

func TestAPassRunsAtOnceThenAtEachChangeAndWake(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// like the state, a new channel for each wait, closed at the next change
	changes := make(chan chan struct{}, 3)
	first := make(chan struct{})
	changes <- first
	changes <- make(chan struct{})
	changes <- make(chan struct{}) // for the wait after the last pass
	wake := make(chan struct{})
	ran := make(chan struct{})
	go Run(ctx, func() <-chan struct{} { return <-changes }, wake, func() time.Duration {
		ran <- struct{}{}
		return time.Hour
	})
	<-ran // at once
	close(first)
	<-ran // the state changed
	wake <- struct{}{}
	<-ran // a wake-up
}

func TestAWaitOfZeroRunsTheNextPassAtOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	passes := 0
	done := make(chan struct{})
	go Run(ctx, nil, nil, func() time.Duration {
		if passes++; passes == 3 {
			close(done)
			return time.Hour
		}
		return 0
	})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the passes did not follow each other at once")
	}
}

func TestRunEndsWithItsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ended := make(chan struct{})
	go func() {
		Run(ctx, nil, nil, func() time.Duration { return time.Hour })
		close(ended)
	}()
	cancel()
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not end")
	}
}

func TestSoonestIsTheDueTimeWithinTheInterval(t *testing.T) {
	for due, want := range map[time.Duration]time.Duration{
		0: Interval, -time.Second: Interval, time.Second: time.Second, time.Hour: Interval,
	} {
		if got := Soonest(due); got != want {
			t.Errorf("Soonest(%v) = %v, want %v", due, got, want)
		}
	}
}
