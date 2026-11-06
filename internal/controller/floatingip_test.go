package controller

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/webcodingcat/clowder/internal/clusterspec"
	"github.com/webcodingcat/clowder/internal/hetzner/hetznertest"
	"github.com/webcodingcat/clowder/internal/hostrole"
	"github.com/webcodingcat/clowder/internal/state"
)

// withServers adds the active hosts node1 and node2 with the servers 41 and
// 42; node0 of newLeader has server 40. No command adds a host without its
// join, so the test writes them in.
func withServers(l *leader) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for id := state.HostID(1); id <= 2; id++ {
		l.state.Hosts[id] = &state.Host{
			ID: id, Name: "node" + string(rune('0'+id)), Roles: hostrole.DefaultInit, ServerID: 40 + int64(id),
			Underlay: underlayOf(id), PublicAddress: publicAddressOf(id),
			WGPublicKey: "wg", CertificateSerial: "s" + string(rune('0'+id)),
			CertificateNotAfter: time.Now().AddDate(1, 0, 0), Phase: state.Active,
		}
	}
}

// underlayOf and publicAddressOf are the addresses of the tests' hosts.
func underlayOf(id state.HostID) netip.Addr { return netip.AddrFrom4([4]byte{10, 0, 0, byte(id + 2)}) }

func publicAddressOf(id state.HostID) netip.Addr {
	return netip.AddrFrom4([4]byte{198, 51, 100, byte(id + 2)})
}

// withHetzner points hetzner.api at a fake of Hetzner's API that expects
// the token, with the servers 40 to 42 of the hosts 0 to 2 and the
// Floating IP of newLeader routed to none.
func withHetzner(t *testing.T, l *leader, token string) *hetznertest.API {
	t.Helper()
	api := hetznertest.New(t, token)
	for id := state.HostID(0); id <= 2; id++ {
		api.AddServer(40+int64(id), underlayOf(id), publicAddressOf(id))
	}
	api.RouteFloatingIP(netip.MustParseAddr("203.0.113.10"), 0)
	l.mu.Lock()
	l.state.Config.Spec.Hetzner.API = api.URL
	l.mu.Unlock()
	return api
}

// holdOnce runs one pass against the fake; the token must arrive opened.
func holdOnce(t *testing.T, l *leader) {
	t.Helper()
	open := func(encrypted state.Encrypted) ([]byte, error) {
		opened, err := openPadded(encrypted)
		return append([]byte("opened-"), opened...), err
	}
	asked, err := holdFloatingIP(context.Background(), l, l.propose, open, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil || !asked {
		t.Fatalf("asked the API %v, %v", asked, err)
	}
}

func TestTheFloatingIPGoesToTheIngressHostAndStays(t *testing.T) {
	l := newLeader(t, 100<<30)
	withServers(l)
	api := withHetzner(t, l, "opened-t")
	ip := netip.MustParseAddr("203.0.113.10")
	holdOnce(t, l)
	l.Read(func(current *state.State) {
		if current.Ingress == nil || *current.Ingress != 0 {
			t.Errorf("ingress %v, want host 0 (the lowest ID)", current.Ingress)
		}
		if !current.Hosts[0].ServerChecked {
			t.Error("host 0's server was not checked first")
		}
	})
	if want := []string{"assign 203.0.113.10 40"}; !slices.Equal(api.Changes(), want) {
		t.Errorf("changes %v, want %v", api.Changes(), want)
	}

	holdOnce(t, l)
	if len(api.Changes()) != 1 {
		t.Errorf("assigned again although the Floating IP is in place: %v", api.Changes())
	}

	api.RouteFloatingIP(ip, 42) // moved by hand
	holdOnce(t, l)
	if got := api.FloatingIPTarget(ip); got != 40 || len(api.Changes()) != 2 {
		t.Errorf("not moved back: on %d, changes %v", got, api.Changes())
	}
}

func TestTheFloatingIPGoesToNoServerOfAnotherHost(t *testing.T) {
	l := newLeader(t, 100<<30)
	api := withHetzner(t, l, "t")
	// server 40 has another private address: host 0 named a stranger
	api.AddServer(40, netip.MustParseAddr("10.0.0.99"), publicAddressOf(0))
	asked, err := holdFloatingIP(context.Background(), l, l.propose, openPadded, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if !asked || err == nil || !strings.Contains(err.Error(), "not the host's underlay 10.0.0.2: it is another server") {
		t.Errorf("asked %v, %v", asked, err)
	}
	if len(api.Changes()) != 0 {
		t.Errorf("changes %v", api.Changes())
	}
	l.Read(func(current *state.State) {
		if current.Ingress != nil || current.Hosts[0].ServerChecked {
			t.Errorf("ingress %v, host 0 %+v", current.Ingress, current.Hosts[0])
		}
	})
}

func TestTheIngressHostIsKeptThenPreferredThenTheLowestID(t *testing.T) {
	host := func(id state.HostID, name string, phase state.HostPhase) *state.Host {
		return &state.Host{ID: id, Name: name, Roles: hostrole.DefaultInit, ServerID: 40 + int64(id), Phase: phase}
	}
	recorded := func(id state.HostID) *state.HostID { return &id }
	hosts := map[state.HostID]*state.Host{
		0: host(0, "node0", state.Joining),
		1: host(1, "node1", state.Active),
		2: host(2, "node2", state.Active),
		3: host(3, "node3", state.Draining),
	}
	for _, test := range []struct {
		name      string
		recorded  *state.HostID
		preferred []string
		want      state.HostID
	}{
		{name: "lowest ID that can", want: 1},
		{name: "preferred over the lowest ID", preferred: []string{"node3", "node2"}, want: 2},
		{name: "recorded over preferred", recorded: recorded(1), preferred: []string{"node2"}, want: 1},
		{name: "a recorded host that cannot", recorded: recorded(3), preferred: []string{"node2"}, want: 2},
	} {
		current := &state.State{Hosts: hosts, Ingress: test.recorded}
		current.Config.Spec.Ingress.PreferredHosts = test.preferred
		if got, ok := ingressHost(current); !ok || got.ID != test.want {
			t.Errorf("%s: got %v, want host %d", test.name, got, test.want)
		}
	}
	if _, ok := ingressHost(&state.State{Hosts: map[state.HostID]*state.Host{0: hosts[0], 3: hosts[3]}}); ok {
		t.Error("a host that is not active was chosen")
	}
	// a checked host goes before a recorded one Hetzner never confirmed
	checked := host(2, "node2", state.Active)
	checked.ServerChecked = true
	current := &state.State{Hosts: map[state.HostID]*state.Host{1: hosts[1], 2: checked}, Ingress: recorded(1)}
	if got, ok := ingressHost(current); !ok || got.ID != 2 {
		t.Errorf("with a checked host: got %v", got)
	}
}

func TestTheIngressHostHasTheIngressRole(t *testing.T) {
	host := func(id state.HostID, roles hostrole.Set) *state.Host {
		return &state.Host{
			ID: id, Name: "node" + string(rune('0'+id)), Roles: roles, ServerID: 40 + int64(id),
			Phase: state.Active, ServerChecked: true,
		}
	}
	recorded := state.HostID(0)
	current := &state.State{Hosts: map[state.HostID]*state.Host{
		0: host(0, hostrole.Set{hostrole.ControlPlane, hostrole.Worker}),
		1: host(1, hostrole.Set{hostrole.Worker}),
		2: host(2, hostrole.DefaultJoin),
	}, Ingress: &recorded}
	current.Config.Spec.Ingress.PreferredHosts = []string{"node1"}
	if got, ok := ingressHost(current); !ok || got.ID != 2 {
		t.Errorf("got %v, want host 2, the only one with ingress", got)
	}
	delete(current.Hosts, 2)
	if got, ok := ingressHost(current); ok {
		t.Errorf("host %d without ingress was chosen", got.ID)
	}
}

// runFloatingIPUntil runs the Floating IP's loop until done holds for the
// conditions, then stops it; a loop asks Hetzner at once when it starts.
func runFloatingIPUntil(t *testing.T, l *leader, open Open, conditions *Conditions, what string, done func() bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		RunFloatingIP(ctx, l, l.propose, open, conditions, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	defer func() {
		cancel()
		<-stopped
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Error(what)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestARefusedHetznerTokenRaisesItsAlertUntilHetznerTakesItAgain(t *testing.T) {
	l := newLeader(t, 100<<30)
	withHetzner(t, l, "t") // the token of the Secret hetzner
	conditions := NewConditions()
	wrongToken := func(state.Encrypted) ([]byte, error) { return []byte("another project's"), nil }
	refused := func() bool {
		_, holds := conditions.Since(hetznerTokenRefused)
		return holds
	}
	runFloatingIPUntil(t, l, wrongToken, conditions, "a token Hetzner refused raised no condition", refused)
	l.Read(func(current *state.State) {
		alert, ok := wantedAlerts(current, conditions, time.Now())[hetznerTokenRefused]
		if !ok || alert.Severity != clusterspec.SeverityCritical {
			t.Errorf("alert %+v, %v", alert, ok)
		}
	})

	runFloatingIPUntil(t, l, openPadded, conditions, "a token Hetzner took again kept the condition",
		func() bool { return !refused() })
	l.Read(func(current *state.State) {
		if _, ok := wantedAlerts(current, conditions, time.Now())[hetznerTokenRefused]; ok {
			t.Error("the alert stayed")
		}
	})
}

func TestTheServiceURLWarningsAreLoggedOncePerConfiguration(t *testing.T) {
	l := newLeader(t, 100<<30)
	var logged bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logged, nil))
	count := func() int { return strings.Count(logged.String(), "acme.directory is set") }

	warned := warnServiceURLs(l, nil, logger)
	if count() != 0 || len(warned) != 0 {
		t.Fatalf("a configuration without service URLs warned: %s", logged.String())
	}
	var config clusterspec.Config
	l.Read(func(current *state.State) { config = current.Config.Spec })
	config.ACME.Directory = "http://ca.lab/directory"
	putConfig(l, config)
	warned = warnServiceURLs(l, warned, logger)
	warned = warnServiceURLs(l, warned, logger)
	if count() != 1 || !strings.Contains(logged.String(), "over http the ACME account's requests") {
		t.Errorf("after one new configuration:\n%s", logged.String())
	}

	config.ACME.Directory = "https://ca.lab/directory"
	putConfig(l, config)
	warnServiceURLs(l, warned, logger)
	if count() != 2 {
		t.Errorf("a configuration with another warning was not logged:\n%s", logged.String())
	}
}
