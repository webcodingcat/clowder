// Package supervisor restarts the stacks' containers. Docker restarts
// nothing itself (every container has restart "no"), so that after a boot
// no container starts before the agent has looked at the cluster; the
// supervisor applies each service's restart rule instead.
package supervisor

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/webcodingcat/clowder/internal/docker"
)

// Docker is what the supervisor needs from Docker; docker.Client is the
// real one.
type Docker interface {
	Events(ctx context.Context, label string) (<-chan docker.Event, <-chan error)
	Start(ctx context.Context, id string) error
	Restart(ctx context.Context, id string) error
}

// Rule is how a service restarts: Compose's restart rule (no, always,
// on-failure[:n], unless-stopped) and whether unhealthy means restart.
type Rule struct {
	Restart          string
	RestartUnhealthy bool
}

// Rules tells the rule of a service of a stack, from the cluster's state;
// ok is false for a container clowder does not know (any more).
type Rules func(stack, service string) (rule Rule, ok bool)

// Backoff is the wait before a restart: Base, doubling up to Max; after
// Stable of running, it starts at Base again.
type Backoff struct {
	Base, Max, Stable time.Duration
}

// DefaultBackoff is 1 s, 2 s, 4 s ... up to 60 s, reset after 10 min.
var DefaultBackoff = Backoff{Base: time.Second, Max: time.Minute, Stable: 10 * time.Minute}

// Supervisor watches Docker's events and restarts what crashed.
type Supervisor struct {
	docker  Docker
	rules   Rules
	backoff Backoff
	// missed is called when the events broke off and are followed again:
	// what died in between went unseen (a restart of Docker kills every
	// container), so the agent starts its stacks as after a boot
	missed func()
	// seen is called after each event, so whoever follows the containers
	// (the routes to them) looks again
	seen   func()
	logger *slog.Logger

	mu sync.Mutex
	// held are the stacks the agent is stopping or recreating itself: the
	// register of wanted stops. A container that dies in one is no crash.
	held map[string]int
	// stopped are containers someone stopped (docker stop): left alone.
	stopped map[string]bool
	// crashes counts each container's restarts since it last ran stably.
	crashes map[string]int
	started map[string]time.Time
}

// New makes a supervisor; missed is called after a break in Docker's
// events, seen after each event.
func New(dockerAPI Docker, rules Rules, backoff Backoff, missed, seen func(), logger *slog.Logger) *Supervisor {
	return &Supervisor{
		docker: dockerAPI, rules: rules, backoff: backoff, missed: missed, seen: seen, logger: logger,
		held: map[string]int{}, stopped: map[string]bool{},
		crashes: map[string]int{}, started: map[string]time.Time{},
	}
}

// Hold registers that the agent itself is about to stop or recreate a
// stack's containers (compose up of a new generation, down, a move); the
// returned function ends that. Holds of one stack may overlap.
func (s *Supervisor) Hold(stack string) (release func()) {
	s.mu.Lock()
	s.held[stack]++
	s.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.held[stack]--; s.held[stack] <= 0 {
				delete(s.held, stack)
			}
		})
	}
}

// Run follows Docker's events of clowder's containers until ctx ends. When
// the stream breaks (Docker restarted), it follows again after a pause.
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
		// docker stop or kill: the container stays down, unless the agent did
		// it; a kill that only signals (a reload with SIGHUP) is no stop
		s.mu.Lock()
		if s.held[stack] == 0 {
			s.stopped[id] = true
		}
		s.mu.Unlock()
	case event.Action == "die":
		s.crashed(ctx, stack, service, id, event.ExitCode)
	case event.Action == "destroy":
		// a new generation makes new containers; forget the old ones
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
	held, stopped := s.held[stack] > 0, s.stopped[id]
	if since, ok := s.started[id]; ok && time.Since(since) >= s.backoff.Stable {
		s.crashes[id] = 0 // it ran stably: count anew
	}
	attempt := s.crashes[id]
	restart := known && !held && !stopped && restarts(rule.Restart, exitCode, attempt)
	if restart {
		s.crashes[id] = attempt + 1
	}
	s.mu.Unlock()
	if !restart {
		return
	}
	delay := s.delay(attempt)
	s.logger.Warn("container crashed; restarting", "stack", stack, "service", service,
		"exit_code", exitCode, "in", delay)
	time.AfterFunc(delay, func() {
		// the stack may be held by now (an update): then it is not ours to start
		s.mu.Lock()
		held := s.held[stack] > 0 || s.stopped[id]
		s.mu.Unlock()
		if held || ctx.Err() != nil {
			return
		}
		err := s.docker.Start(ctx, id)
		switch {
		case err == nil, errors.Is(err, docker.ErrNoContainer):
			// a removed container is not ours to start
		default:
			// a start that fails counts as another crash: tried again later
			s.logger.Error("restart a crashed container", "stack", stack, "service", service, "error", err)
			s.crashed(ctx, stack, service, id, exitCode)
		}
	})
}

// endsContainer tells whether a kill's signal is docker stop's or docker
// kill's (SIGTERM, SIGKILL, SIGINT), not one a service takes as a command.
func endsContainer(signal int) bool {
	return signal == int(syscall.SIGTERM) || signal == int(syscall.SIGKILL) || signal == int(syscall.SIGINT)
}

// unhealthy restarts a container whose health check fails, unless its
// service says not to (x-clowder.restart_unhealthy: false). It backs off
// like a crash: a container that stays unhealthy is not restarted at every
// health check.
func (s *Supervisor) unhealthy(ctx context.Context, stack, service, id string) {
	rule, known := s.rules(stack, service)
	s.mu.Lock()
	held := s.held[stack] > 0
	if since, ok := s.started[id]; ok && time.Since(since) >= s.backoff.Stable {
		s.crashes[id] = 0
	}
	attempt := s.crashes[id]
	restart := known && !held && rule.RestartUnhealthy
	if restart {
		s.crashes[id] = attempt + 1
	}
	s.mu.Unlock()
	if !restart {
		return
	}
	delay := s.delay(attempt)
	s.logger.Warn("container unhealthy; restarting", "stack", stack, "service", service, "in", delay)
	time.AfterFunc(delay, func() {
		s.mu.Lock()
		held := s.held[stack] > 0 || s.stopped[id]
		s.mu.Unlock()
		if held || ctx.Err() != nil {
			return
		}
		if err := s.docker.Restart(ctx, id); err != nil && !errors.Is(err, docker.ErrNoContainer) {
			s.logger.Error("restart an unhealthy container", "stack", stack, "service", service, "error", err)
		}
	})
}

// delay is the wait before restart number attempt (from 0).
func (s *Supervisor) delay(attempt int) time.Duration {
	delay := s.backoff.Base
	for range attempt {
		delay *= 2
		if delay >= s.backoff.Max {
			return s.backoff.Max
		}
	}
	return delay
}

// restarts applies Compose's restart rule to a container that exited; the
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
