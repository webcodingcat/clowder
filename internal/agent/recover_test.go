package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/webcodingcat/clowder/internal/api"
	"github.com/webcodingcat/clowder/internal/clusterspec"
	"github.com/webcodingcat/clowder/internal/hoster"
	"github.com/webcodingcat/clowder/internal/raftnode"
	"github.com/webcodingcat/clowder/internal/state"
)

// retiringHoster records what recover asks of the hoster.
type retiringHoster struct {
	token string
	calls []string
	// offFails fails the next power off of a server, once
	offFails map[string]error
}

func (h *retiringHoster) connect(_ clusterspec.Hoster, token string) (hoster.Hoster, error) {
	h.token = token
	return h, nil
}

func (h *retiringHoster) SetRetired(_ context.Context, serverRef, serial string) error {
	h.calls = append(h.calls, "retire "+serverRef+" "+serial)
	return nil
}

func (h *retiringHoster) PowerOff(_ context.Context, serverRef string) error {
	h.calls = append(h.calls, "off "+serverRef)
	err := h.offFails[serverRef]
	delete(h.offFails, serverRef)
	return err
}

func (h *retiringHoster) ServerAddresses(_ context.Context, serverRef string) ([]netip.Addr, error) {
	h.calls = append(h.calls, "check "+serverRef)
	return []netip.Addr{netip.MustParseAddr("10.0.0.4")}, nil
}

// The rest is not recover's.
func (h *retiringHoster) RetiredSerial(context.Context, string) (string, error) { return "", nil }

func (h *retiringHoster) PowerOn(context.Context, string) error { return nil }

func (h *retiringHoster) FloatingIPTarget(context.Context, netip.Addr) (string, error) {
	return "", nil
}

func (h *retiringHoster) AssignFloatingIP(context.Context, netip.Addr, string) error { return nil }

// recordedStorage records the peers recover drops.
type recordedStorage struct {
	dropped [][]int
	// fails fails the next drop, once
	fails error
}

func (s *recordedStorage) DropPeers(_ context.Context, peers []int) error {
	s.dropped = append(s.dropped, peers)
	err := s.fails
	s.fails = nil
	return err
}

// lostCluster makes a host, node0, whose Raft log has a cluster of three
// hosts with a hoster: hosts 1 and 2 on servers 41 and 42. Its agent does
// not run.
func lostCluster(t *testing.T) Files {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	loopback := netip.MustParseAddr("127.0.0.1")
	files := Files{Dir: t.TempDir()}
	err := Init(context.Background(), files, InitOptions{
		BootstrapKey: testBootstrapKey, Name: "node0", Underlay: loopback, ServerRef: "1000001",
		NodeAddress: loopback, RaftPort: freePort(t), APIPort: freePort(t),
	}, &fakeNetwork{}, logger)
	if err != nil {
		t.Fatal(err)
	}
	host, err := files.readHost()
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := files.readCredentials()
	if err != nil {
		t.Fatal(err)
	}
	node, err := raftnode.Open(raftnode.Config{
		Dir: files.raftDir(), ID: host.ID, Bind: host.raftAddress(), Advertise: host.raftAddress(),
		Certificate: credentials.certificate, CA: credentials.ca, Logger: logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = node.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := node.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	token, err := credentials.dataKey.Seal([]byte("hoster-token"))
	if err != nil {
		t.Fatal(err)
	}
	commit := func(kind, actor string, payload any) {
		t.Helper()
		cmd, err := state.NewCommand(kind, time.Now(), actor, payload)
		if err == nil {
			_, err = node.Propose(ctx, cmd)
		}
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
	}
	commit(state.KindApplyConfig, state.UserActor("alice"), state.ApplyConfig{
		Spec: clusterspec.Config{
			Name: "prod", Endpoints: []string{"127.0.0.1"},
			Hoster:         clusterspec.Hoster{Type: "hetzner", Token: "hoster.token"},
			Ingress:        clusterspec.Ingress{FloatingIP: "203.0.113.10"},
			StorageClasses: map[string]clusterspec.StorageClass{"fast": {Pool: "vg-fast/thin"}},
		},
		Secrets: map[string]state.SealedSecret{"hoster.token": {Value: token, Digest: "d"}},
	})
	for id, serverRef := range map[state.HostID]string{1: "41", 2: "42"} {
		name := "node" + string(rune('0'+id))
		commit(state.KindAddJoinToken, state.UserActor("alice"), state.AddJoinToken{
			ID: name, Hash: strings.Repeat("ab", 32), TTL: time.Hour,
		})
		commit(state.KindAddHost, state.LeaderActor, state.AddHost{Token: name, Host: state.Host{
			ID: id, Name: name, Underlay: netip.AddrFrom4([4]byte{10, 0, 0, byte(id + 2)}), WGPublicKey: "wg",
			CertificateSerial: "s" + name, CertificateNotAfter: time.Now().AddDate(1, 0, 0),
			ServerRef: serverRef, ServerRefChecked: true,
		}})
		commit(state.KindSetHostPhase, state.LeaderActor, state.SetHostPhase{Host: id, Phase: state.Active})
	}
	return files
}

// recoverParts are fakes for recover; no other host leads.
func recoverParts(client *retiringHoster, driver *recordedStorage, service *[]string) RecoverParts {
	return RecoverParts{
		Storage: driver, Hoster: client.connect,
		Service: func(_ context.Context, action string) error {
			*service = append(*service, action)
			return nil
		},
		KnowsLeader: func(context.Context, string, tls.Certificate, *x509.Certificate) (bool, error) { return false, nil },
	}
}

func TestRecoverRetiresTheOthersAndGoesOnAlone(t *testing.T) {
	files := lostCluster(t)
	client, driver := &retiringHoster{}, &recordedStorage{}
	var service []string
	var out bytes.Buffer
	options := RecoverOptions{In: strings.NewReader("prod\n"), Out: &out}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := Recover(context.Background(), files, recoverParts(client, driver, &service), options, logger); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if client.token != "hoster-token" {
		t.Errorf("the hoster's token: %q", client.token)
	}
	// the label first: a server that is off but not labeled could come back
	want := []string{"retire 41 snode1", "off 41", "retire 42 snode2", "off 42"}
	if !slices.Equal(client.calls, want) {
		t.Errorf("hoster calls %q, want %q", client.calls, want)
	}
	if !slices.Equal(service, []string{"stop", "start"}) || len(driver.dropped) != 1 || !slices.Equal(driver.dropped[0], []int{1, 2}) {
		t.Errorf("service %q, dropped peers %v", service, driver.dropped)
	}
	for _, text := range []string{"node1 (server 41, active)", "Type the cluster's name", "only host now"} {
		if !strings.Contains(out.String(), text) {
			t.Errorf("the output lacks %q:\n%s", text, out.String())
		}
	}
	current, err := raftnode.ReadState(files.raftDir())
	if err != nil {
		t.Fatal(err)
	}
	if current.Hosts[1].Phase != state.Removed || current.Hosts[2].Phase != state.Removed || current.RecoveredAt.IsZero() {
		t.Errorf("hosts %v, %v; recovered at %v", current.Hosts[1].Phase, current.Hosts[2].Phase, current.RecoveredAt)
	}

	// again, as after a failure: nothing left to retire or to ask
	client.calls, service = nil, nil
	options = RecoverOptions{In: strings.NewReader(""), Out: io.Discard}
	if err := Recover(context.Background(), files, recoverParts(client, driver, &service), options, logger); err != nil {
		t.Fatal(err)
	}
	if len(client.calls) != 0 || !slices.Equal(service, []string{"stop", "start"}) {
		t.Errorf("a second recover: hoster %q, service %q", client.calls, service)
	}
}

func TestRecoverChangesNothingWhenItIsRefused(t *testing.T) {
	// knowsIf has the host with the underlay 10.0.0.<2+id> know a leader
	knowsIf := func(id state.HostID, err error) knowsLeader {
		return func(_ context.Context, address string, _ tls.Certificate, _ *x509.Certificate) (bool, error) {
			return strings.HasPrefix(address, fmt.Sprintf("10.0.0.%d:", id+2)), err
		}
	}
	for _, test := range []struct {
		name, answer, want string
		knows              knowsLeader
	}{
		{"another host knows a leader", "prod\n", "host node2 knows a leader", knowsIf(2, nil)},
		{
			"another host answers without saying", "prod\n", "host node1 answers, but not whether it knows a leader",
			knowsIf(-1, errors.New("certificate refused")),
		},
		{"the cluster's name is not typed", "test\n", "not confirmed", knowsIf(-1, nil)},
	} {
		t.Run(test.name, func(t *testing.T) {
			files := lostCluster(t)
			client, driver := &retiringHoster{}, &recordedStorage{}
			var service []string
			parts := recoverParts(client, driver, &service)
			parts.KnowsLeader = test.knows
			options := RecoverOptions{In: strings.NewReader(test.answer), Out: io.Discard}
			err := Recover(context.Background(), files, parts, options, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Errorf("got %v, want %q", err, test.want)
			}
			// the agent runs again, and nothing else changed
			if len(client.calls) != 0 || len(driver.dropped) != 0 || !slices.Equal(service, []string{"stop", "start"}) {
				t.Errorf("hoster %q, dropped %v, service %q", client.calls, driver.dropped, service)
			}
			current, err := raftnode.ReadState(files.raftDir())
			if err != nil || current.Hosts[1].Phase != state.Active {
				t.Errorf("the state changed: %v", err)
			}
		})
	}
}

func TestRecoverNamesTheHostsItDoesNotReach(t *testing.T) {
	files := lostCluster(t)
	client, driver := &retiringHoster{}, &recordedStorage{}
	var service []string
	parts := recoverParts(client, driver, &service)
	parts.KnowsLeader = func(context.Context, string, tls.Certificate, *x509.Certificate) (bool, error) {
		return false, fmt.Errorf("%w: connection refused", api.ErrNotReached)
	}
	var out bytes.Buffer
	options := RecoverOptions{Yes: true, Out: &out}
	if err := Recover(context.Background(), files, parts, options, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Not reached, so not asked whether they lead: node1, node2") {
		t.Errorf("output:\n%s", out.String())
	}
}

func TestRecoverHasNothingToDoForASingleHostOrWithoutAHoster(t *testing.T) {
	one := &state.State{Keys: &state.ClusterKeys{}, Hosts: map[state.HostID]*state.Host{0: {ID: 0, Phase: state.Active}}}
	if _, err := hostsToRetire(one, Host{ID: 0, Name: "node0"}); err == nil || !strings.Contains(err.Error(), "nothing to recover") {
		t.Errorf("a single host: %v", err)
	}
	one.RecoveredAt = time.Now()
	if gone, err := hostsToRetire(one, Host{ID: 0, Name: "node0"}); err != nil || len(gone) != 0 {
		t.Errorf("a single host after a recover: %v, %v", gone, err)
	}
	one.Hosts[1] = &state.Host{ID: 1, Phase: state.Active}
	if _, err := hostsToRetire(one, Host{ID: 0, Name: "node0"}); err == nil || !strings.Contains(err.Error(), "names no hoster") {
		t.Errorf("without a hoster: %v", err)
	}
}

func TestRecoverEndsWhereItFailedWhenItRunsAgain(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, test := range []struct {
		name   string
		breaks func(client *retiringHoster, driver *recordedStorage)
	}{
		{"the hoster fails at the second host", func(client *retiringHoster, _ *recordedStorage) {
			client.offFails = map[string]error{"42": errors.New("locked")}
		}},
		{"DRBD fails after the remap", func(_ *retiringHoster, driver *recordedStorage) {
			driver.fails = errors.New("drbdsetup timed out")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			files := lostCluster(t)
			client, driver := &retiringHoster{}, &recordedStorage{}
			test.breaks(client, driver)
			var service []string
			options := RecoverOptions{Yes: true, Out: io.Discard}
			err := Recover(context.Background(), files, recoverParts(client, driver, &service), options, logger)
			if err == nil || !strings.Contains(err.Error(), "clowderd.service is stopped: run recover again") {
				t.Errorf("the first run: %v", err)
			}
			// it changed something: the agent stays stopped
			if !slices.Equal(service, []string{"stop"}) {
				t.Errorf("service %q", service)
			}
			service = nil
			if err := Recover(context.Background(), files, recoverParts(client, driver, &service), options, logger); err != nil {
				t.Fatal(err)
			}
			current, err := raftnode.ReadState(files.raftDir())
			if err != nil || current.Hosts[2].Phase != state.Removed || !slices.Equal(service, []string{"stop", "start"}) {
				t.Errorf("after the second run: %v, service %q", err, service)
			}
		})
	}
}

// knowsLeader is RecoverParts.KnowsLeader.
type knowsLeader = func(ctx context.Context, address string, certificate tls.Certificate, ca *x509.Certificate) (bool, error)
