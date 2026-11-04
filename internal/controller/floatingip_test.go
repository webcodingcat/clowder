package controller

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/webcodingcat/clowder/internal/clusterspec"
	"github.com/webcodingcat/clowder/internal/hoster"
	"github.com/webcodingcat/clowder/internal/state"
)

// fakeHoster routes the Floating IP where it is told; its servers 41 and
// 42 have the underlays of withServers' hosts, unless servers says else.
type fakeHoster struct {
	target  string
	assigns []string
	servers map[string][]netip.Addr
	// powered are the power actions in order, "off 41", "on 41"
	powered []string
	// powerFails makes every power action fail
	powerFails error
}

func (f *fakeHoster) PowerOff(_ context.Context, serverRef string) error {
	f.powered = append(f.powered, "off "+serverRef)
	return f.powerFails
}

func (f *fakeHoster) PowerOn(_ context.Context, serverRef string) error {
	f.powered = append(f.powered, "on "+serverRef)
	return f.powerFails
}

func (f *fakeHoster) ServerAddresses(_ context.Context, serverRef string) ([]netip.Addr, error) {
	if f.servers != nil {
		return f.servers[serverRef], nil
	}
	switch serverRef {
	case "41":
		return []netip.Addr{netip.MustParseAddr("10.0.0.3")}, nil
	case "42":
		return []netip.Addr{netip.MustParseAddr("10.0.0.4")}, nil
	}
	return nil, nil
}

func (f *fakeHoster) FloatingIPTarget(context.Context, netip.Addr) (string, error) {
	return f.target, nil
}

func (f *fakeHoster) AssignFloatingIP(_ context.Context, _ netip.Addr, serverRef string) error {
	f.assigns = append(f.assigns, serverRef)
	f.target = serverRef
	return nil
}

// withServers adds the active hosts node1 and node2 with server refs 41 and
// 42; node0 of newLeader has none. No command adds a host until phase 2
// brings the join, so the test writes them in.
func withServers(l *leader) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for id := state.HostID(1); id <= 2; id++ {
		l.state.Hosts[id] = &state.Host{
			ID: id, Name: "node" + string(rune('0'+id)), Underlay: netip.AddrFrom4([4]byte{10, 0, 0, byte(id + 2)}),
			WGPublicKey: "wg", CertificateSerial: "s" + string(rune('0'+id)), ServerRef: "4" + string(rune('0'+id)),
			CertificateNotAfter: time.Now().AddDate(1, 0, 0), Phase: state.Active,
		}
	}
}

// holdOnce runs one pass against the fake hoster; the token must arrive
// opened.
func holdOnce(t *testing.T, l *leader, fake *fakeHoster) {
	t.Helper()
	open := func(sealed state.Sealed) ([]byte, error) { return append([]byte("opened-"), sealed...), nil }
	connect := func(config clusterspec.Hoster, token string) (hoster.Hoster, error) {
		if config.Type != "hetzner" || token != "opened-t" {
			t.Errorf("connect %v with %q", config, token)
		}
		return fake, nil
	}
	asked, err := holdFloatingIP(context.Background(), l, l.propose, open, connect, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil || !asked {
		t.Fatalf("asked the API %v, %v", asked, err)
	}
}

func TestTheFloatingIPGoesToTheIngressHostAndStays(t *testing.T) {
	l := newLeader(t, 100<<30)
	withServers(l)
	fake := &fakeHoster{}
	holdOnce(t, l, fake)
	l.Read(func(current *state.State) {
		if current.Ingress == nil || *current.Ingress != 1 {
			t.Errorf("ingress %v, want host 1 (the lowest ID with a server ref)", current.Ingress)
		}
		if !current.Hosts[1].ServerRefChecked {
			t.Error("host 1's server ref was not checked first")
		}
	})
	if len(fake.assigns) != 1 || fake.assigns[0] != "41" {
		t.Errorf("assigns %v", fake.assigns)
	}

	holdOnce(t, l, fake)
	if len(fake.assigns) != 1 {
		t.Errorf("assigned again although the Floating IP is in place: %v", fake.assigns)
	}

	fake.target = "99" // moved by hand
	holdOnce(t, l, fake)
	if len(fake.assigns) != 2 || fake.assigns[1] != "41" {
		t.Errorf("not moved back: %v", fake.assigns)
	}
}

func TestTheFloatingIPGoesToNoServerOfAnotherHost(t *testing.T) {
	l := newLeader(t, 100<<30)
	withServers(l)
	// server 41 has another private address: host 1 named a stranger
	fake := &fakeHoster{servers: map[string][]netip.Addr{"41": {netip.MustParseAddr("10.0.0.99")}}}
	open := func(sealed state.Sealed) ([]byte, error) { return sealed, nil }
	connect := func(clusterspec.Hoster, string) (hoster.Hoster, error) { return fake, nil }
	asked, err := holdFloatingIP(context.Background(), l, l.propose, open, connect, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if !asked || err == nil || !strings.Contains(err.Error(), "not the host's 10.0.0.3: it is another server") {
		t.Errorf("asked %v, %v", asked, err)
	}
	if len(fake.assigns) != 0 {
		t.Errorf("assigned to %v", fake.assigns)
	}
	l.Read(func(current *state.State) {
		if current.Ingress != nil || current.Hosts[1].ServerRefChecked {
			t.Errorf("ingress %v, host 1 %+v", current.Ingress, current.Hosts[1])
		}
	})
}

func TestTheIngressHostIsKeptThenPreferredThenTheLowestID(t *testing.T) {
	host := func(id state.HostID, name, serverRef string, phase state.HostPhase) *state.Host {
		return &state.Host{ID: id, Name: name, ServerRef: serverRef, Phase: phase}
	}
	recorded := func(id state.HostID) *state.HostID { return &id }
	hosts := map[state.HostID]*state.Host{
		0: host(0, "node0", "", state.Active), // no server ref
		1: host(1, "node1", "41", state.Active),
		2: host(2, "node2", "42", state.Active),
		3: host(3, "node3", "43", state.Draining),
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
		t.Error("a host without a server ref or not active was chosen")
	}
	// a checked host goes before a recorded one the hoster never confirmed
	checked := host(2, "node2", "42", state.Active)
	checked.ServerRefChecked = true
	current := &state.State{Hosts: map[state.HostID]*state.Host{1: hosts[1], 2: checked}, Ingress: recorded(1)}
	if got, ok := ingressHost(current); !ok || got.ID != 2 {
		t.Errorf("with a checked host: got %v", got)
	}
}

// SetRetired and RetiredSerial are clowderd recover's and the agent's start;
// the leader never uses them.
func (f *fakeHoster) SetRetired(context.Context, string, string) error {
	return errors.New("not the leader's")
}

func (f *fakeHoster) RetiredSerial(context.Context, string) (string, error) {
	return "", errors.New("not the leader's")
}
