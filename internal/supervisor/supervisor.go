// Package supervisor restarts the stacks' containers. Docker restarts
// nothing itself, because every container has restart "no". So after a
// boot, no container starts before the agent has checked the cluster. The
// supervisor applies each service's restart rule instead.
package supervisor

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/webcodingcat/clowder/internal/docker"
)

// Docker is what the supervisor needs from Docker. docker.Client is the
// real implementation.
type Docker interface {
	Events(ctx context.Context, label string) (<-chan docker.Event, <-chan error)
	Start(ctx context.Context, id string) error
	Restart(ctx context.Context, id string) error
}

// Rule says how a service restarts: Compose's restart rule (no, always,
// on-failure[:n], unless-stopped), and whether an unhealthy container is
// restarted.
type Rule struct {
	Restart          string
	RestartUnhealthy bool
}

// Rules returns the rule of a stack's service from the cluster's state. ok
// is false for a container that clowder does not know (or no longer knows).
type Rules func(stack, service string) (rule Rule, ok bool)

// Backoff is the wait before a restart: none for the first restart, then
// Base, doubling up to Max. After a container has run for Stable, its next
// crash counts as the first again.
type Backoff struct {
	Base, Max, Stable time.Duration
}

// DefaultBackoff restarts right away, then waits 1 s, 2 s, 4 s ... up to
// 60 s, and resets after 10 min.
var DefaultBackoff = Backoff{Base: time.Second, Max: time.Minute, Stable: 10 * time.Minute}

// Supervisor watches Docker's events and restarts what crashed.
type Supervisor struct {
	docker  Docker
	rules   Rules
	backoff Backoff
	// missed is called when the event stream broke off and is followed
	// again. Containers that died in between went unseen. This happens when
	// Docker restarts: live-restore keeps the containers running, but not
	// their events. So the agent brings up its stacks as after a boot.
	missed func()
	// seen is called after each event, so that whoever follows the
	// containers (the routes to them) checks them again
	seen   func()
	logger *slog.Logger

	mu sync.Mutex
	// paused are the stacks that the agent itself is stopping or
	// recreating, counted per stack. A container that dies in such a stack
	// has not crashed.
	paused map[string]int
	// stopped are containers that someone stopped (docker stop). The
	// supervisor leaves them alone.
	stopped map[string]bool
	// crashes counts each container's restarts since it last ran stably.
	crashes map[string]int
	started map[string]time.Time
	// restarts counts every restart of each service since the agent
	// started, for /metrics. A deleted stack's count stays until the agent
	// restarts, because a counter must not go back.
	restarts map[Service]int
}

// Service names a service by its stack (the Compose project) and name.
type Service struct {
	Stack, Name string
}

// New creates a supervisor. missed is called after a break in Docker's
// events, and seen is called after each event.
func New(dockerAPI Docker, rules Rules, backoff Backoff, missed, seen func(), logger *slog.Logger) *Supervisor {
	return &Supervisor{
		docker: dockerAPI, rules: rules, backoff: backoff, missed: missed, seen: seen, logger: logger,
		paused: map[string]int{}, stopped: map[string]bool{},
		crashes: map[string]int{}, started: map[string]time.Time{}, restarts: map[Service]int{},
	}
}

// Restarts returns how often the supervisor restarted each service (crashed
// or unhealthy) since the agent started.
func (s *Supervisor) Restarts() map[Service]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return maps.Clone(s.restarts)
}

// Pause stops supervising a stack while the agent itself stops or recreates
// its containers (compose up of a new generation, down, a move). The
// returned function resumes supervising. Pauses of one stack may overlap.
func (s *Supervisor) Pause(stack string) (resume func()) {
	s.mu.Lock()
	s.paused[stack]++
	s.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.paused[stack]--; s.paused[stack] <= 0 {
				delete(s.paused, stack)
			}
		})
	}
}

// Run follows Docker's events for clowder's containers until ctx ends. When
// the stream breaks (Docker restarted), Run follows it again after a pause.
func (s *Supervisor) Run(ctx context.Context, label string) {
	for following := false; ctx.Err() == nil; following = true {
		events, failed := s.docker.Events(ctx, label)
		if following {
			s.missed()
		}
		for event := range events {
			s.handle(ctx, event)
			s.seen()
		}
		select {
		case err := <-failed:
			s.logger.Warn("Docker's events broke off; following again", "error", err)
		default:
		}
		select {
		case <-ctx.Done():
		case <-time.After(s.backoff.Base):
		}
	}
}

// handle decides about one event.
func (s *Supervisor) handle(ctx context.Context, event docker.Event) {
	stack, service := event.Labels[docker.ProjectLabel], event.Labels[docker.ServiceLabel]
	id := event.ContainerID
	switch {
	case event.Action == "start":
		s.mu.Lock()
		s.started[id] = time.Now()
		delete(s.stopped, id)
		s.mu.Unlock()
	case event.Action == "stop" || (event.Action == "kill" && endsContainer(event.Signal)):
		// docker stop or kill: the container stays down, unless the agent
		// stopped it. A kill that only sends a signal (a reload with SIGHUP)
		// is not a stop.
		s.mu.Lock()
		if s.paused[stack] == 0 {
			s.stopped[id] = true
		}
		s.mu.Unlock()
	case event.Action == "die":
		s.crashed(ctx, stack, service, id, event.ExitCode)
	case event.Action == "destroy":
		// a new generation makes new containers, so forget the old ones
		s.mu.Lock()
		delete(s.started, id)
		delete(s.stopped, id)
		delete(s.crashes, id)
		s.mu.Unlock()
	case strings.HasPrefix(event.Action, "health_status: unhealthy"):
		s.unhealthy(ctx, stack, service, id)
	}
}

// crashed restarts a container that died, if its rule says so.
func (s *Supervisor) crashed(ctx context.Context, stack, service, id string, exitCode int) {
	rule, known := s.rules(stack, service)
	s.mu.Lock()
	paused, stopped := s.paused[stack] > 0, s.stopped[id]
	if since, ok := s.started[id]; ok && time.Since(since) >= s.backoff.Stable {
		s.crashes[id] = 0 // it ran stably: start counting again
	}
	attempt := s.crashes[id]
	restart := known && !paused && !stopped && restarts(rule.Restart, exitCode, attempt)
	if restart {
		s.crashes[id] = attempt + 1
		s.restarts[Service{stack, service}]++
	}
	s.mu.Unlock()
	if !restart {
		return
	}
	delay := s.delay(attempt)
	s.logger.Warn("container crashed; restarting", "stack", stack, "service", service,
		"exit_code", exitCode, "in", delay)
	time.AfterFunc(delay, func() {
		// the stack may be paused by now (an update), and then the
		// supervisor must not start it
		s.mu.Lock()
		leftAlone := s.paused[stack] > 0 || s.stopped[id]
		s.mu.Unlock()
		if leftAlone || ctx.Err() != nil {
			return
		}
		err := s.docker.Start(ctx, id)
		switch {
		case err == nil, errors.Is(err, docker.ErrNoContainer):
			// a removed container is not the supervisor's to start
		default:
			// a failed start counts as another crash and is tried again
			// later
			s.logger.Error("restart a crashed container", "stack", stack, "service", service, "error", err)
			s.crashed(ctx, stack, service, id, exitCode)
		}
	})
}

// endsContainer tells whether a kill's signal is one that docker stop or
// docker kill sends (SIGTERM, SIGKILL, SIGINT), rather than one that a
// service takes as a command.
func endsContainer(signal int) bool {
	return signal == int(syscall.SIGTERM) || signal == int(syscall.SIGKILL) || signal == int(syscall.SIGINT)
}

// unhealthy restarts a container whose health check fails, unless its
// service says not to (restartUnhealthy: false in its stack). It backs off
// as for a crash, so a container that stays unhealthy is not restarted at
// every health check.
func (s *Supervisor) unhealthy(ctx context.Context, stack, service, id string) {
	rule, known := s.rules(stack, service)
	s.mu.Lock()
	paused := s.paused[stack] > 0
	if since, ok := s.started[id]; ok && time.Since(since) >= s.backoff.Stable {
		s.crashes[id] = 0
	}
	attempt := s.crashes[id]
	restart := known && !paused && rule.RestartUnhealthy
	if restart {
		s.crashes[id] = attempt + 1
		s.restarts[Service{stack, service}]++
	}
	s.mu.Unlock()
	if !restart {
		return
	}
	delay := s.delay(attempt)
	s.logger.Warn("container unhealthy; restarting", "stack", stack, "service", service, "in", delay)
	time.AfterFunc(delay, func() {
		s.mu.Lock()
		leftAlone := s.paused[stack] > 0 || s.stopped[id]
		s.mu.Unlock()
		if leftAlone || ctx.Err() != nil {
			return
		}
		if err := s.docker.Restart(ctx, id); err != nil && !errors.Is(err, docker.ErrNoContainer) {
			s.logger.Error("restart an unhealthy container", "stack", stack, "service", service, "error", err)
		}
	})
}

// delay is the wait before restart number attempt (counted from 0). The
// first restart comes right away, so a service that crashed once is back
// within 2 s (a goal that the live test checks). A service that crashes
// again waits, so a crash loop does not spin.
func (s *Supervisor) delay(attempt int) time.Duration {
	if attempt == 0 {
		return 0
	}
	delay := s.backoff.Base
	for range attempt - 1 {
		delay *= 2
		if delay >= s.backoff.Max {
			return s.backoff.Max
		}
	}
	return delay
}

// restarts applies Compose's restart rule to a container that exited. The
// default (no rule) is unless-stopped.
func restarts(rule string, exitCode, attempt int) bool {
	name, limit, hasLimit := strings.Cut(rule, ":")
	switch name {
	case "no":
		return false
	case "on-failure":
		if exitCode == 0 {
			return false
		}
		if !hasLimit {
			return true
		}
		maximum, err := strconv.Atoi(limit)
		return err == nil && attempt < maximum
	default: // always, unless-stopped, empty
		return true
	}
}
