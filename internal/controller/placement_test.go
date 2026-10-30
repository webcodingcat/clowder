package controller

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/webcodingcat/clowder/internal/clusterdir"
	"github.com/webcodingcat/clowder/internal/state"
)

// leader is a state that applies what is proposed, as the leader would.
type leader struct {
	t       *testing.T
	mu      sync.Mutex
	state   *state.State
	changed chan struct{}
}

func (l *leader) Read(use func(current *state.State)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	use(l.state)
}

func (l *leader) Changed() <-chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.changed
}

func (l *leader) IsLeader() bool { return true }

func (l *leader) propose(_ context.Context, cmd state.Command) (uint64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cmd.At = time.Now()
	index := l.state.Index + 1
	if err := l.state.Apply(index, cmd); err != nil {
		l.t.Errorf("%s refused: %v", cmd.Kind, err)
		return index, err
	}
	close(l.changed)
	l.changed = make(chan struct{})
	return index, nil
}

func (l *leader) apply(kind, actor string, payload any) {
	cmd, err := state.NewCommand(kind, time.Now(), actor, payload)
	if err != nil {
		l.t.Fatal(err)
	}
	if _, err := l.propose(context.Background(), cmd); err != nil {
		l.t.Fatal(err)
	}
}

// newLeader is a bootstrapped cluster with one host whose pool vg-fast/thin
// has the given size, and a stack shop applied with a 1G volume db.
func newLeader(t *testing.T, poolSize int64) *leader {
	l := &leader{t: t, state: &state.State{}, changed: make(chan struct{})}
	l.apply(state.KindBootstrap, state.LeaderActor, state.Bootstrap{
		Keys:    state.ClusterKeys{CACertificate: []byte("ca"), CAKey: state.Sealed("k"), AgeIdentity: state.Sealed("a")},
		JoinKey: state.Sealed("j"),
		Host: state.Host{
			ID: 0, Name: "node0", Underlay: netip.MustParseAddr("10.0.0.2"), WGPublicKey: "wg",
			CertificateSerial: "s0",
		},
	})
	l.apply(state.KindApplyConfig, state.UserActor("alice"), state.ApplyConfig{
		Spec: clusterdir.Config{
			Name: "prod", Seeds: []string{"10.0.0.2"},
			Hoster:         clusterdir.Hoster{Type: "hetzner", Token: clusterdir.SecretRef{Secret: "token"}},
			Ingress:        clusterdir.Ingress{FloatingIP: "203.0.113.10"},
			StorageClasses: map[string]clusterdir.StorageClass{"fast": {Pool: "vg-fast/thin"}},
		},
		Secrets: map[string]state.Sealed{"token": state.Sealed("t")},
	})
	l.apply(state.KindReportHost, state.HostActor(0), state.ReportHost{
		Host: 0, Version: "1.0.0",
		Pools: map[string]state.Pool{"vg-fast/thin": {Name: "vg-fast/thin", SizeBytes: poolSize}},
	})
	l.apply(state.KindCreateStack, state.UserActor("alice"), state.CreateStack{Name: "shop", Owner: state.OwnerCLI})
	l.apply(state.KindPutStack, state.UserActor("alice"), state.PutStack{
		Name: "shop", Owner: state.OwnerCLI, ConfigRevision: 1,
		Source: "name: shop\nservices: {app: {image: x}}\nvolumes:\n  db:\n    x-clowder: {class: fast, size: 1G, replicas: 1}\n",
	})
	return l
}

// runPlacement runs the placement until the test ends.
func runPlacement(t *testing.T, l *leader) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go RunPlacement(ctx, l, l.propose, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// waitFor fails the test unless the state comes to hold what check says.
func waitFor(t *testing.T, l *leader, what string, check func(current *state.State) bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var done bool
		l.Read(func(current *state.State) { done = check(current) })
		if done {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error(what)
}

func TestAPendingStackIsPlacedWithItsVolumes(t *testing.T) {
	l := newLeader(t, 100<<30)
	runPlacement(t, l)
	waitFor(t, l, "the stack was not placed", func(current *state.State) bool {
		stack := current.Stacks["shop"]
		return stack.Host != nil && stack.Status == state.StackStarting && current.Volumes["shop/db"] != nil &&
			stack.WaitReason == ""
	})
}

// newPlacer is the placement of l, for passes one by one, so a test knows
// how many ran.
func newPlacer(l *leader) *placer {
	return &placer{
		cluster: l, propose: l.propose, logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		overCapacity: map[state.HostID]bool{},
	}
}

func TestAStackThatFitsNowhereShowsWhyUntilItFits(t *testing.T) {
	l := newLeader(t, 512<<20)
	placement := newPlacer(l)
	for range 3 {
		placement.place(context.Background())
	}
	l.Read(func(current *state.State) {
		if reason := current.Stacks["shop"].WaitReason; reason != "node0: volume db (1G, class fast) fits no pool" {
			t.Errorf("reason %q", reason)
		}
		written := 0
		for _, entry := range current.Audit {
			if entry.Kind == state.KindSetStackWaitReason {
				written++
			}
		}
		if written != 1 {
			t.Errorf("three passes wrote the reason %d times, want once", written)
		}
	})

	l.apply(state.KindReportHost, state.HostActor(0), state.ReportHost{
		Host: 0, Version: "1.0.1",
		Pools: map[string]state.Pool{"vg-fast/thin": {Name: "vg-fast/thin", SizeBytes: 100 << 30}},
	})
	placement.place(context.Background())
	l.Read(func(current *state.State) {
		if stack := current.Stacks["shop"]; stack.Host == nil || stack.WaitReason != "" || !stack.WaitingSince.IsZero() {
			t.Errorf("after the pool grew: host %v, reason %q since %v", stack.Host, stack.WaitReason, stack.WaitingSince)
		}
	})
}

func TestAServicePushedOutComesBackAfterItFitAMinute(t *testing.T) {
	l := newLeader(t, 100<<30)
	// 1000M for services after the system reserve; shop's app takes
	// 600M, the new stack's web 500M at a higher priority
	l.apply(state.KindReportHost, state.HostActor(0), state.ReportHost{
		Host: 0, Version: "1.0.0", Capacity: state.Resources{MemoryBytes: 1512 << 20, CPUs: 2.25},
		Pools: map[string]state.Pool{"vg-fast/thin": {Name: "vg-fast/thin", SizeBytes: 100 << 30}},
	})
	low, high := 200, 800
	l.apply(state.KindPutStack, state.UserActor("alice"), state.PutStack{
		Name: "shop", Owner: state.OwnerCLI, ConfigRevision: 1,
		Source:   "name: shop\nservices: {app: {image: x}}\n",
		Services: map[string]state.Service{"app": {Priority: clusterdir.Priority{Value: &low}, MemoryBytes: 600 << 20}},
	})
	placement := newPlacer(l)
	placement.place(context.Background())
	l.apply(state.KindCreateStack, state.UserActor("alice"), state.CreateStack{Name: "web", Owner: state.OwnerCLI})
	l.apply(state.KindPutStack, state.UserActor("alice"), state.PutStack{
		Name: "web", Owner: state.OwnerCLI, ConfigRevision: 1,
		Source:   "name: web\nservices: {web: {image: x}}\n",
		Services: map[string]state.Service{"web": {Priority: clusterdir.Priority{Value: &high}, MemoryBytes: 500 << 20}},
	})
	placement.place(context.Background())
	l.Read(func(current *state.State) {
		if fmt.Sprint(current.Stacks["shop"].PushedOut) != "[app]" || current.Stacks["web"].Host == nil {
			t.Fatalf("shop pushed out %v, web on %v", current.Stacks["shop"].PushedOut, current.Stacks["web"].Host)
		}
	})

	l.apply(state.KindDeleteStack, state.UserActor("alice"), state.DeleteStack{Name: "web"})
	if again := placement.place(context.Background()); again <= 0 || again > restartAfter {
		t.Errorf("look again in %v", again)
	}
	l.Read(func(current *state.State) {
		if len(current.Stacks["shop"].PushedOut) != 1 {
			t.Error("app came back before it fit a minute")
		}
	})
	placement.fittingSince = time.Now().Add(-restartAfter) // a minute went by
	placement.place(context.Background())
	l.Read(func(current *state.State) {
		if len(current.Stacks["shop"].PushedOut) != 0 {
			t.Errorf("app is still pushed out: %v", current.Stacks["shop"].PushedOut)
		}
	})
}
