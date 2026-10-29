package controller

import (
	"context"
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

func TestAPendingStackIsPlacedWithItsVolumes(t *testing.T) {
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
			Name: "prod", Seeds: []string{"10.0.0.2"}, Hoster: "hetzner",
			HosterToken:    clusterdir.SecretRef{Secret: "token"},
			Ingress:        clusterdir.Ingress{FloatingIP: "203.0.113.10"},
			StorageClasses: map[string]clusterdir.StorageClass{"fast": {Pool: "vg-fast/thin"}},
		},
		Secrets: map[string]state.Sealed{"token": state.Sealed("t")},
	})
	l.apply(state.KindReportHost, state.HostActor(0), state.ReportHost{
		Host: 0, Version: "1.0.0",
		Pools: map[string]state.Pool{"vg-fast/thin": {Name: "vg-fast/thin", SizeBytes: 100 << 30}},
	})
	l.apply(state.KindPutStack, state.UserActor("alice"), state.PutStack{
		Name: "shop", Owner: state.OwnerCLI,
		Source: "services: {app: {image: x}}\nvolumes:\n  db:\n    x-clowder: {class: fast, size: 1G, replicas: 1}\n",
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go RunPlacement(ctx, l, l.propose, slog.New(slog.NewTextHandler(io.Discard, nil)))

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var placed bool
		l.Read(func(current *state.State) {
			stack := current.Stacks["shop"]
			placed = stack.Host != nil && stack.Status == state.StackStarting && current.Volumes["shop/db"] != nil
		})
		if placed {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("the stack was not placed")
}
