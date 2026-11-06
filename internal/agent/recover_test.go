package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
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
	"github.com/webcodingcat/clowder/internal/hetzner/hetznertest"
	"github.com/webcodingcat/clowder/internal/hostrole"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/raftnode"
	"github.com/webcodingcat/clowder/internal/resource"
	"github.com/webcodingcat/clowder/internal/state"
)

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
// hosts in a Hetzner project (the fake it returns): hosts 1 and 2 on
// servers 41 and 42. Its agent does not run. The hosts' private network is
// the loopback's.
func lostCluster(t *testing.T) (Files, *hetznertest.API) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	loopback := netip.MustParseAddr("127.0.0.1")
	files := Files{Dir: t.TempDir()}
	err := Init(context.Background(), files, InitOptions{
		ClusterName: testClusterName, Name: "node0", Roles: hostrole.DefaultInit, Underlay: loopback,
		PublicAddress: netip.MustParseAddr("198.51.100.2"), ServerID: 1000001,
		NodeAddress: loopback, RaftPort: freePort(t), APIPort: freePort(t),
	}, &fakeNetwork{}, logger)
	if err != nil {
		t.Fatal(err)
	}
	host, err := files.readHost()
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := files.readCredentials(host.Roles)
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
	token, err := credentials.dataKey.Encrypt([]byte("hetzner-token"))
	if err != nil {
		t.Fatal(err)
	}
	project := hetznertest.New(t, "hetzner-token")
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
	encode := func(spec any) []byte {
		t.Helper()
		encoded, err := json.Marshal(spec)
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}
	secret := encode(state.EncryptedSecret{Data: map[string]state.Encrypted{"token": token}, Digest: "d"})
	config := encode(clusterspec.Config{
		Hetzner: clusterspec.Hetzner{
			TokenSecretRef: names.SecretKeyRef{Name: "hetzner", Key: "token"}, API: project.URL,
		},
		PrivateNetwork: "127.0.0.0/8",
		Ingress:        clusterspec.Ingress{FloatingIP: "203.0.113.10"},
		StorageClasses: map[string]clusterspec.StorageClass{"fast": {Pool: "vg-fast/thin"}},
	})
	commit(state.KindChangeResources, state.CLIActor("admin"), state.ChangeResources{Changes: []state.ResourceChange{
		{Put: &state.PutResource{
			Kind: resource.KindSecret, Namespace: names.System, Name: "hetzner", Owner: state.OwnerCLI, Spec: secret,
		}},
		{Put: &state.PutResource{
			Kind: resource.KindClusterConfiguration, Name: resource.ClusterConfigurationName, Owner: state.OwnerCLI,
			Spec: config,
		}},
	}})
	for id := state.HostID(1); id <= 2; id++ {
		name := "node" + string(rune('0'+id))
		underlay, public := netip.AddrFrom4([4]byte{127, 0, 0, byte(id + 2)}), netip.AddrFrom4([4]byte{198, 51, 100, byte(id + 2)})
		project.AddServer(40+int64(id), underlay, public)
		commit(state.KindAddJoinToken, state.CLIActor("admin"), state.AddJoinToken{
			ID: name, Hash: strings.Repeat("ab", 32), TTL: time.Hour, Roles: hostrole.DefaultInit,
		})
		commit(state.KindAddHost, state.LeaderActor, state.AddHost{Token: name, Host: state.Host{
			ID: id, Name: name, Roles: hostrole.DefaultInit, Underlay: underlay, PublicAddress: public, WGPublicKey: "wg",
			CertificateSerial: "s" + name, CertificateNotAfter: time.Now().AddDate(1, 0, 0),
			ServerID: 40 + int64(id), ServerChecked: true,
		}})
		commit(state.KindSetHostPhase, state.LeaderActor, state.SetHostPhase{Host: id, Phase: state.Active})
	}
	return files, project
}

// recoverParts are fakes for recover; no other host leads.
func recoverParts(driver *recordedStorage, service *[]string) RecoverParts {
	return RecoverParts{
		Storage: driver,
		Service: func(_ context.Context, action string) error {
			*service = append(*service, action)
			return nil
		},
		KnowsLeader: func(context.Context, string, tls.Certificate, *x509.Certificate) (bool, error) { return false, nil },
	}
}

func TestRecoverRetiresTheOthersAndGoesOnAlone(t *testing.T) {
	files, project := lostCluster(t)
	driver := &recordedStorage{}
	var service []string
	var out bytes.Buffer
	options := RecoverOptions{In: strings.NewReader("node0\n"), Out: &out}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := Recover(context.Background(), files, recoverParts(driver, &service), options, logger); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	// the label first: a server that is off but not labeled could come back
	want := []string{"label 41 clowder-retired=snode1", "poweroff 41", "label 42 clowder-retired=snode2", "poweroff 42"}
	if !slices.Equal(project.Changes(), want) {
		t.Errorf("changes at Hetzner %q, want %q", project.Changes(), want)
	}
	if !slices.Equal(service, []string{"stop", "start"}) || len(driver.dropped) != 1 || !slices.Equal(driver.dropped[0], []int{1, 2}) {
		t.Errorf("service %q, dropped peers %v", service, driver.dropped)
	}
	for _, text := range []string{"node1 (server 41, active)", "Type this host's name", "Recover cluster prod", "only host now"} {
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
	service = nil
	options = RecoverOptions{In: strings.NewReader(""), Out: io.Discard}
	if err := Recover(context.Background(), files, recoverParts(driver, &service), options, logger); err != nil {
		t.Fatal(err)
	}
	if len(project.Changes()) != len(want) || !slices.Equal(service, []string{"stop", "start"}) {
		t.Errorf("a second recover: changes at Hetzner %q, service %q", project.Changes(), service)
	}
}

func TestRecoverChangesNothingWhenItIsRefused(t *testing.T) {
	// knowsIf has the host with the underlay 127.0.0.<2+id> know a leader
	knowsIf := func(id state.HostID, err error) knowsLeader {
		return func(_ context.Context, address string, _ tls.Certificate, _ *x509.Certificate) (bool, error) {
			return strings.HasPrefix(address, fmt.Sprintf("127.0.0.%d:", id+2)), err
		}
	}
	for _, test := range []struct {
		name, answer, want string
		knows              knowsLeader
	}{
		{"another host knows a leader", "node0\n", "host node2 knows a leader", knowsIf(2, nil)},
		{
			"another host answers without saying", "node0\n", "host node1 answers, but not whether it knows a leader",
			knowsIf(-1, errors.New("certificate refused")),
		},
		{"the host's name is not typed", "prod\n", "not confirmed", knowsIf(-1, nil)},
	} {
		t.Run(test.name, func(t *testing.T) {
			files, project := lostCluster(t)
			driver := &recordedStorage{}
			var service []string
			parts := recoverParts(driver, &service)
			parts.KnowsLeader = test.knows
			options := RecoverOptions{In: strings.NewReader(test.answer), Out: io.Discard}
			err := Recover(context.Background(), files, parts, options, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Errorf("got %v, want %q", err, test.want)
			}
			// the agent runs again, and nothing else changed
			if len(project.Changes()) != 0 || len(driver.dropped) != 0 || !slices.Equal(service, []string{"stop", "start"}) {
				t.Errorf("changes at Hetzner %q, dropped %v, service %q", project.Changes(), driver.dropped, service)
			}
			current, err := raftnode.ReadState(files.raftDir())
			if err != nil || current.Hosts[1].Phase != state.Active {
				t.Errorf("the state changed: %v", err)
			}
		})
	}
}

func TestRecoverNamesTheHostsItDoesNotReach(t *testing.T) {
	files, _ := lostCluster(t)
	var service []string
	parts := recoverParts(&recordedStorage{}, &service)
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

func TestRecoverHasNothingToDoForASingleHostOrBeforeTheConfiguration(t *testing.T) {
	one := &state.State{Keys: &state.ClusterKeys{}, Hosts: map[state.HostID]*state.Host{0: {ID: 0, Phase: state.Active}}}
	if _, err := hostsToRetire(one, Host{ID: 0, Name: "node0"}); err == nil || !strings.Contains(err.Error(), "nothing to recover") {
		t.Errorf("a single host: %v", err)
	}
	one.RecoveredAt = time.Now()
	if gone, err := hostsToRetire(one, Host{ID: 0, Name: "node0"}); err != nil || len(gone) != 0 {
		t.Errorf("a single host after a recover: %v, %v", gone, err)
	}
	one.Hosts[1] = &state.Host{ID: 1, Phase: state.Active}
	if _, err := hostsToRetire(one, Host{ID: 0, Name: "node0"}); err == nil || !strings.Contains(err.Error(), "no ClusterConfiguration names the Hetzner token") {
		t.Errorf("before the configuration: %v", err)
	}
}

func TestRecoverEndsWhereItFailedWhenItRunsAgain(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, test := range []struct {
		name   string
		breaks func(project *hetznertest.API, driver *recordedStorage)
	}{
		{"Hetzner fails at the second host", func(project *hetznertest.API, _ *recordedStorage) {
			project.FailActions(42, 1)
		}},
		{"DRBD fails after the remap", func(_ *hetznertest.API, driver *recordedStorage) {
			driver.fails = errors.New("drbdsetup timed out")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			files, project := lostCluster(t)
			driver := &recordedStorage{}
			test.breaks(project, driver)
			var service []string
			options := RecoverOptions{Yes: true, Out: io.Discard}
			err := Recover(context.Background(), files, recoverParts(driver, &service), options, logger)
			if err == nil || !strings.Contains(err.Error(), "clowderd.service is stopped: run recover again") {
				t.Errorf("the first run: %v", err)
			}
			// it changed something: the agent stays stopped
			if !slices.Equal(service, []string{"stop"}) {
				t.Errorf("service %q", service)
			}
			service = nil
			if err := Recover(context.Background(), files, recoverParts(driver, &service), options, logger); err != nil {
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
