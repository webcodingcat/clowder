package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/webcodingcat/clowder/internal/clusterspec"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/resource"
	"github.com/webcodingcat/clowder/internal/stackspec"
	"github.com/webcodingcat/clowder/internal/state"
	"github.com/webcodingcat/clowder/internal/state/statetest"
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

// SharedCopy copies afresh each time: what the tests check is that the
// loops read the copy, not the state under its lock, and never write into it.
func (l *leader) SharedCopy() (*state.State, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return statetest.ReadOnlyCopy(l.t, l.state)
}

func (l *leader) Changed() <-chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.changed
}

func (l *leader) IsLeader() bool { return true }

func (l *leader) VerifyLeader(context.Context) error { return nil }

func (l *leader) propose(_ context.Context, cmd state.Command) (uint64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cmd.At = time.Now()
	index := l.state.Index + 1
	if _, err := l.state.Apply(index, cmd); err != nil {
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
		Keys:        state.ClusterKeys{CACertificate: []byte("ca"), CAKey: state.Encrypted("k"), SealingKey: state.Encrypted("a")},
		ClusterName: "prod",
		Host: state.Host{
			ID: 0, Name: "node0", ServerID: 40, Underlay: netip.MustParseAddr("10.0.0.2"),
			PublicAddress: netip.MustParseAddr("198.51.100.2"), WGPublicKey: "wg",
			CertificateSerial: "s0", CertificateNotAfter: time.Now().AddDate(1, 0, 0),
		},
	})
	putSystemSecret(l, "hetzner", map[string]string{"token": "t"})
	putConfig(l, clusterspec.Config{
		Hetzner:        clusterspec.Hetzner{TokenSecretRef: names.SecretKeyRef{Name: "hetzner", Key: "token"}},
		PrivateNetwork: "10.0.0.0/16",
		Ingress:        clusterspec.Ingress{FloatingIP: "203.0.113.10"},
		StorageClasses: map[string]clusterspec.StorageClass{"fast": {Pool: "vg-fast/thin"}},
	})
	l.apply(state.KindReportHost, state.HostActor(0), state.ReportHost{
		Host: 0, Version: "1.0.0", Capacity: state.Resources{MemoryBytes: 4 << 30, CPUs: 4},
		Pools: map[string]state.Pool{"vg-fast/thin": {Name: "vg-fast/thin", SizeBytes: poolSize}},
	})
	putResource(l, resource.KindVolume, "db", resource.VolumeSpec{Class: "fast", Size: 1 << 30, Replicas: 1})
	putResource(l, resource.KindStack, "shop", stackspec.Spec{Services: map[string]stackspec.Service{
		"app": {Image: "x", Volumes: []stackspec.Mount{{Volume: "db", Path: "/db"}}},
	}})
	return l
}

// putResource applies a resource of the namespace default, as clowder
// apply does.
func putResource(l *leader, kind, name string, spec any) {
	l.t.Helper()
	putResourceIn(l, names.Default, kind, name, spec)
}

// putResourceIn applies a resource of a namespace ("" for a kind that
// lives in none).
func putResourceIn(l *leader, namespace, kind, name string, spec any) {
	l.t.Helper()
	encoded, err := json.Marshal(spec)
	if err != nil {
		l.t.Fatal(err)
	}
	l.apply(state.KindChangeResources, state.CLIActor("admin"), state.ChangeResources{Changes: []state.ResourceChange{{
		Put: &state.PutResource{Kind: kind, Namespace: namespace, Name: name, Owner: state.OwnerCLI, Spec: encoded},
	}}})
}

// encryptedPadding makes a test's value as long as the state takes an
// encrypted one to be (state.ClearSize), so it may stand in for one.
var encryptedPadding = strings.Repeat("~", 200)

// openPadded opens a value putSystemSecret "encrypted": it drops the padding.
func openPadded(encrypted state.Encrypted) ([]byte, error) {
	opened, ok := bytes.CutPrefix(encrypted, []byte(encryptedPadding))
	if !ok {
		return nil, errors.New("not encrypted")
	}
	return opened, nil
}

// putSystemSecret applies a Secret of clowder-system; each value is
// "encrypted" by padding it, which openPadded drops again.
func putSystemSecret(l *leader, name string, values map[string]string) {
	l.t.Helper()
	secret := state.EncryptedSecret{Data: map[string]state.Encrypted{}, Digest: "d"}
	for key, value := range values {
		secret.Data[key] = state.Encrypted(encryptedPadding + value)
	}
	putResourceIn(l, names.System, resource.KindSecret, name, secret)
}

// putConfig applies the cluster's configuration.
func putConfig(l *leader, config clusterspec.Config) {
	l.t.Helper()
	putResourceIn(l, "", resource.KindClusterConfiguration, resource.ClusterConfigurationName, config)
}

// deleteResource deletes a resource of the namespace default.
func deleteResource(l *leader, kind, name string) {
	l.t.Helper()
	l.apply(state.KindChangeResources, state.CLIActor("admin"), state.ChangeResources{Changes: []state.ResourceChange{{
		Delete: &state.DeleteResource{Kind: kind, Namespace: names.Default, Name: name, Owner: state.OwnerCLI},
	}}})
}

// reserving is a stack of one service that reserves memory at a priority.
func reserving(service, priority, memory string) stackspec.Spec {
	return stackspec.Spec{Priority: priority, Services: map[string]stackspec.Service{service: {
		Image: "x", Resources: stackspec.Resources{Reserve: &stackspec.Amount{Memory: memory}},
	}}}
}

// runPlacement runs the placement until the test ends.
func runPlacement(t *testing.T, l *leader) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go RunPlacement(ctx, l, l.propose, NewConditions(), slog.New(slog.NewTextHandler(io.Discard, nil)))
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
		stack := current.Stacks["default/shop"]
		return stack.Host != nil && stack.Status == state.StackStarting &&
			current.Volumes["default/db"].Status == state.VolumeOK && stack.WaitReason == ""
	})
}

// newPlacer is the placement of l, for passes one by one, so a test knows
// how many ran.
func newPlacer(l *leader) *placer {
	return &placer{
		cluster: l, propose: l.propose, logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		conditions: NewConditions(),
	}
}

func TestAStackThatFitsNowhereShowsWhyUntilItFits(t *testing.T) {
	l := newLeader(t, 512<<20)
	placement := newPlacer(l)
	for range 3 {
		placement.pass(context.Background())
	}
	l.Read(func(current *state.State) {
		if reason := current.Stacks["default/shop"].WaitReason; reason != "node0: volume db (1G, class fast) fits no pool" {
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
		Host: 0, Version: "1.0.1", Capacity: state.Resources{MemoryBytes: 4 << 30, CPUs: 4},
		Pools: map[string]state.Pool{"vg-fast/thin": {Name: "vg-fast/thin", SizeBytes: 100 << 30}},
	})
	placement.pass(context.Background())
	l.Read(func(current *state.State) {
		if stack := current.Stacks["default/shop"]; stack.Host == nil || stack.WaitReason != "" || !stack.WaitingSince.IsZero() {
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
	putResource(l, resource.KindStack, "shop", reserving("app", "200", "600M"))
	placement := newPlacer(l)
	placement.pass(context.Background())
	putResource(l, resource.KindStack, "web", reserving("web", "800", "500M"))
	placement.pass(context.Background())
	l.Read(func(current *state.State) {
		if fmt.Sprint(current.Stacks["default/shop"].PushedOut) != "[app]" || current.Stacks["default/web"].Host == nil {
			t.Fatalf("shop pushed out %v, web on %v", current.Stacks["default/shop"].PushedOut, current.Stacks["default/web"].Host)
		}
	})

	deleteResource(l, resource.KindStack, "web")
	if again := placement.pass(context.Background()); again <= 0 || again > comeBackAfter {
		t.Errorf("look again in %v", again)
	}
	l.Read(func(current *state.State) {
		if len(current.Stacks["default/shop"].PushedOut) != 1 {
			t.Error("app came back before it fit a minute")
		}
	})
	placement.fittingSince = time.Now().Add(-comeBackAfter) // a minute went by
	placement.pass(context.Background())
	l.Read(func(current *state.State) {
		if len(current.Stacks["default/shop"].PushedOut) != 0 {
			t.Errorf("app is still pushed out: %v", current.Stacks["default/shop"].PushedOut)
		}
	})
}

func TestAPlacementPlannedOnAnOlderStackIsPlannedAgain(t *testing.T) {
	l := newLeader(t, 100<<30)
	placement := newPlacer(l)
	// a user's apply comes in between the leader's plan and its commit of
	// the stack's host
	changed := stackspec.Spec{Services: map[string]stackspec.Service{
		"app":   {Image: "x", Volumes: []stackspec.Mount{{Volume: "db", Path: "/db"}}},
		"cache": {Image: "x"},
	}}
	var refused error
	placement.propose = func(ctx context.Context, cmd state.Command) (uint64, error) {
		if cmd.Kind != state.KindSetStackStatus || refused != nil {
			return l.propose(ctx, cmd)
		}
		putResource(l, resource.KindStack, "shop", changed)
		l.mu.Lock()
		defer l.mu.Unlock()
		index := l.state.Index + 1
		_, refused = l.state.Apply(index, cmd)
		return index, refused
	}
	placement.pass(context.Background())
	if !errors.Is(refused, state.ErrStale) {
		t.Fatalf("want the stack's host refused as stale, got %v", refused)
	}
	l.Read(func(current *state.State) {
		if stack := current.Stacks["default/shop"]; stack.Host != nil || current.Volumes["default/db"].Status != state.VolumeOK {
			t.Errorf("after the stale pass: stack on %v, volume %s", stack.Host, current.Volumes["default/db"].Status)
		}
	})
	placement.pass(context.Background())
	l.Read(func(current *state.State) {
		if stack := current.Stacks["default/shop"]; stack.Host == nil || stack.Generation != 2 {
			t.Errorf("the next pass placed generation %d on %v", stack.Generation, stack.Host)
		}
	})
}
