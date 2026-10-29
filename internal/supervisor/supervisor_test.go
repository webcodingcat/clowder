package supervisor

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/webcodingcat/clowder/internal/docker"
)

// fakeDocker hands the supervisor events and records what it starts.
type fakeDocker struct {
	events    chan docker.Event
	mu        sync.Mutex
	started   []string
	restarted []string
}

func (f *fakeDocker) Events(context.Context, string) (<-chan docker.Event, <-chan error) {
	return f.events, make(chan error)
}

func (f *fakeDocker) Start(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, id)
	return nil
}

func (f *fakeDocker) Restart(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restarted = append(f.restarted, id)
	return nil
}

func (f *fakeDocker) counts() (started, restarted int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.started), len(f.restarted)
}

// rules of the test stack "shop": app restarts unless stopped, job only on
// failure (twice), once never, db must not restart when unhealthy.
func rules(stack, service string) (Rule, bool) {
	if stack != "shop" {
		return Rule{}, false
	}
	switch service {
	case "app":
		return Rule{Restart: "unless-stopped", RestartUnhealthy: true}, true
	case "job":
		return Rule{Restart: "on-failure:2"}, true
	case "once":
		return Rule{Restart: "no"}, true
	case "db":
		return Rule{Restart: "always", RestartUnhealthy: false}, true
	}
	return Rule{}, false
}

// running starts a supervisor with a tiny backoff.
func running(t *testing.T) (*Supervisor, *fakeDocker) {
	t.Helper()
	fake := &fakeDocker{events: make(chan docker.Event)}
	supervisor := New(fake, rules, Backoff{Base: time.Millisecond, Max: 4 * time.Millisecond, Stable: time.Hour},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go supervisor.Run(ctx, "clowder.stack")
	return supervisor, fake
}

func event(action, service string, exitCode int) docker.Event {
	return docker.Event{
		Action: action, ContainerID: "id-" + service, ExitCode: exitCode,
		Labels: map[string]string{stackLabel: "shop", serviceLabel: service},
	}
}

// settle gives restarts after a backoff time to happen.
func settle() { time.Sleep(30 * time.Millisecond) }

func TestACrashedContainerIsRestarted(t *testing.T) {
	_, fake := running(t)
	fake.events <- event("die", "app", 137)
	settle()
	if started, _ := fake.counts(); started != 1 {
		t.Errorf("%d starts, want 1", started)
	}
}

func TestWantedStopsAreNoCrashes(t *testing.T) {
	supervisor, fake := running(t)
	release := supervisor.Hold("shop")
	fake.events <- event("die", "app", 0)
	settle()
	release()
	fake.events <- event("kill", "app", 0) // docker stop by hand: kill comes before die
	fake.events <- event("die", "app", 143)
	settle()
	if started, _ := fake.counts(); started != 0 {
		t.Errorf("%d starts of containers that were stopped on purpose", started)
	}
	fake.events <- event("start", "app", 0) // started again by hand: watched again
	fake.events <- event("die", "app", 1)
	settle()
	if started, _ := fake.counts(); started != 1 {
		t.Errorf("%d starts after a crash of a container started by hand", started)
	}
}

func TestRestartRulesOfCompose(t *testing.T) {
	_, fake := running(t)
	fake.events <- event("die", "once", 1) // restart: no
	fake.events <- event("die", "job", 0)  // on-failure, but no failure
	settle()
	if started, _ := fake.counts(); started != 0 {
		t.Fatalf("%d starts, want none", started)
	}
	for range 3 {
		fake.events <- event("die", "job", 1) // on-failure:2: two restarts, not three
		settle()
	}
	if started, _ := fake.counts(); started != 2 {
		t.Errorf("%d starts of on-failure:2, want 2", started)
	}
	fake.events <- event("die", "stranger", 1) // a service clowder does not know
	settle()
	if started, _ := fake.counts(); started != 2 {
		t.Errorf("an unknown container was started")
	}
}

func TestUnhealthyContainersRestartUnlessTheyMayNot(t *testing.T) {
	_, fake := running(t)
	fake.events <- event("health_status: unhealthy", "app", 0)
	fake.events <- event("health_status: unhealthy", "db", 0)
	settle()
	if _, restarted := fake.counts(); restarted != 1 {
		t.Errorf("%d restarts, want 1 (app, not db)", restarted)
	}
}

func TestBackoffDoublesUpToItsMaximum(t *testing.T) {
	supervisor := New(nil, rules, DefaultBackoff, slog.New(slog.NewTextHandler(io.Discard, nil)))
	want := []time.Duration{
		time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
		16 * time.Second, 32 * time.Second, time.Minute, time.Minute,
	}
	for attempt, delay := range want {
		if got := supervisor.delay(attempt); got != delay {
			t.Errorf("attempt %d: %s, want %s", attempt, got, delay)
		}
	}
}
