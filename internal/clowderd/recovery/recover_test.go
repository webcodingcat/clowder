package recovery

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
	"net"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/webcodingcat/clowder/internal/api"
	"github.com/webcodingcat/clowder/internal/clowderd/hostfiles"
	"github.com/webcodingcat/clowder/internal/clowderd/initialize"
	"github.com/webcodingcat/clowder/internal/clusterspec"
	"github.com/webcodingcat/clowder/internal/hetzner/hetznertest"
	"github.com/webcodingcat/clowder/internal/hostrole"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/raftnode"
	"github.com/webcodingcat/clowder/internal/resource"
	"github.com/webcodingcat/clowder/internal/state"
	"github.com/webcodingcat/clowder/internal/storage"
)

// recordedStorage records the peers recover drops. Its DRBD has no
// resource up.
type recordedStorage struct {
	dropped [][]int
	// fails fails the next drop, once
	fails error
}

func (s *recordedStorage) DiskStates(context.Context) (map[string]string, error) {
	return map[string]string{}, nil
}

func (s *recordedStorage) DropPeers(_ context.Context, peers []int) error {
	s.dropped = append(s.dropped, peers)
	err := s.fails
	s.fails = nil
	return err
}

// fakeNetwork sets up no interface.
type fakeNetwork struct{}

func (fakeNetwork) SetUpInterface(context.Context, netip.Addr) error { return nil }

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

// lostCluster makes a host, node0, whose Raft log has a cluster of three
// hosts with every role in a Hetzner project (the fake it returns): hosts 1
// and 2 on servers 41 and 42. Its agent does not run. The hosts' private
// network is the loopback's.
func lostCluster(t *testing.T) (hostfiles.Files, *hetznertest.API) {
	t.Helper()
	return lostClusterWith(t, hostrole.DefaultInit)
}

// lostClusterWith is lostCluster with host 2's roles.
func lostClusterWith(t *testing.T, rolesOfTwo hostrole.Set) (hostfiles.Files, *hetznertest.API) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	loopback := netip.MustParseAddr("127.0.0.1")
	files := hostfiles.Files{Dir: t.TempDir()}
	err := initialize.Init(context.Background(), files, initialize.Options{
		ClusterName: "prod", Name: "node0", Roles: hostrole.DefaultInit, Underlay: loopback,
		PublicAddress: netip.MustParseAddr("198.51.100.2"), ServerID: 1000001,
		NodeAddress: loopback, RaftPort: freePort(t), APIPort: freePort(t),
	}, fakeNetwork{}, logger)
	if err != nil {
		t.Fatal(err)
	}
	host, err := files.ReadHost()
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := files.ReadCredentials(host.Roles)
	if err != nil {
		t.Fatal(err)
	}
	node, err := raftnode.Open(raftnode.Config{
		Dir: files.RaftDir(), ID: host.ID, Bind: host.RaftAddress(), Advertise: host.RaftAddress(),
		Certificate: credentials.Certificate, CA: credentials.CA, Logger: logger,
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
	token, err := credentials.DataKey.Encrypt([]byte("hetzner-token"))
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
		roles := hostrole.DefaultInit
		if id == 2 {
			roles = rolesOfTwo
		}
		commit(state.KindAddJoinToken, state.CLIActor("admin"), state.AddJoinToken{
			ID: name, Hash: strings.Repeat("ab", 32), TTL: time.Hour, Roles: roles,
		})
		commit(state.KindAddHost, state.LeaderActor, state.AddHost{Token: name, Host: state.Host{
			ID: id, Name: name, Roles: roles, Underlay: underlay, PublicAddress: public, WGPublicKey: "wg",
			CertificateSerial: "s" + name, CertificateNotAfter: time.Now().AddDate(1, 0, 0),
			ServerID: 40 + int64(id), ServerChecked: true,
		}})
		commit(state.KindSetHostPhase, state.LeaderActor, state.SetHostPhase{Host: id, Phase: state.Active})
	}
	return files, project
}

// recoverParts are fakes for recover; hosts answers for the other hosts.
func recoverParts(driver *recordedStorage, service *[]string, hosts *fakeOtherHosts) Parts {
	return Parts{
		Storage: driver,
		StopService: func(context.Context) error {
			*service = append(*service, "stop")
			return nil
		},
		StartService: func(context.Context) error {
			*service = append(*service, "start")
			return nil
		},
		Hosts: func(tls.Certificate, *x509.Certificate) OtherHosts { return hosts },
	}
}

// fakeOtherHosts answers for the other hosts of lostCluster, by their
// underlay 127.0.0.<2+id>. A host without an answer takes no connection.
type fakeOtherHosts struct {
	answers map[state.HostID]hostAnswer
	// rejoined holds the retired hosts that each host was told of
	rejoined map[state.HostID][]state.HostID
}

type hostAnswer struct {
	knows bool
	err   error // KnowsLeader's
	disks map[string]string
	// copiesErr and rejoinErr are the errors of Copies and Rejoin
	copiesErr, rejoinErr error
}

func (f *fakeOtherHosts) host(address string) state.HostID {
	host, _, _ := net.SplitHostPort(address)
	return state.HostID(netip.MustParseAddr(host).As4()[3] - 2)
}

func (f *fakeOtherHosts) KnowsLeader(_ context.Context, address string) (bool, error) {
	answer, ok := f.answers[f.host(address)]
	if !ok {
		return false, fmt.Errorf("%w: connection refused", api.ErrNotReached)
	}
	return answer.knows, answer.err
}

func (f *fakeOtherHosts) Copies(_ context.Context, address string) (map[string]string, error) {
	answer := f.answers[f.host(address)]
	return answer.disks, answer.copiesErr
}

func (f *fakeOtherHosts) Rejoin(_ context.Context, address string, retired []state.HostID) error {
	if err := f.answers[f.host(address)].rejoinErr; err != nil {
		return err
	}
	if f.rejoined == nil {
		f.rejoined = map[state.HostID][]state.HostID{}
	}
	f.rejoined[f.host(address)] = retired
	return nil
}

func TestRecoverRetiresTheOthersAndGoesOnAlone(t *testing.T) {
	files, project := lostCluster(t)
	driver := &recordedStorage{}
	var service []string
	var out bytes.Buffer
	options := Options{In: strings.NewReader("node0\n"), Out: &out}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	hosts := &fakeOtherHosts{}
	if err := Recover(context.Background(), files, recoverParts(driver, &service, hosts), options, logger); err != nil {
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
	for _, text := range []string{
		"node1 (server 41, active)", "node2 (server 42, active)", "Type this host's name", "Recover cluster prod",
		"only control-plane host now",
	} {
		if !strings.Contains(out.String(), text) {
			t.Errorf("the output lacks %q:\n%s", text, out.String())
		}
	}
	current, err := raftnode.ReadState(files.RaftDir())
	if err != nil {
		t.Fatal(err)
	}
	if current.Hosts[1].Phase != state.Removed || current.Hosts[2].Phase != state.Removed || current.RecoveredAt.IsZero() {
		t.Errorf("hosts %v, %v; recovered at %v", current.Hosts[1].Phase, current.Hosts[2].Phase, current.RecoveredAt)
	}

	// again, as after a failure: nothing left to retire or to ask
	service = nil
	options = Options{In: strings.NewReader(""), Out: io.Discard}
	if err := Recover(context.Background(), files, recoverParts(driver, &service, hosts), options, logger); err != nil {
		t.Fatal(err)
	}
	if len(project.Changes()) != len(want) || !slices.Equal(service, []string{"stop", "start"}) {
		t.Errorf("a second recover: changes at Hetzner %q, service %q", project.Changes(), service)
	}
}

// TestRecoverKeepsAWorkerThatAnswers loses control-plane host 1. Worker 2
// answers: it stays, and it rejoins before this host's agent starts.
func TestRecoverKeepsAWorkerThatAnswers(t *testing.T) {
	files, project := lostClusterWith(t, hostrole.DefaultJoin)
	driver := &recordedStorage{}
	var service []string
	var out bytes.Buffer
	hosts := &fakeOtherHosts{answers: map[state.HostID]hostAnswer{2: {}}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	options := Options{Yes: true, Out: &out}
	if err := Recover(context.Background(), files, recoverParts(driver, &service, hosts), options, logger); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if want := []string{"label 41 clowder-retired=snode1", "poweroff 41"}; !slices.Equal(project.Changes(), want) {
		t.Errorf("changes at Hetzner %q, want %q", project.Changes(), want)
	}
	if len(driver.dropped) != 1 || !slices.Equal(driver.dropped[0], []int{1}) {
		t.Errorf("dropped peers %v, want host 1's", driver.dropped)
	}
	if retired := hosts.rejoined[2]; !slices.Equal(retired, []state.HostID{1}) {
		t.Errorf("the worker was told of the retired hosts %v, want host 1", retired)
	}
	for _, text := range []string{"workers answer and stay", "  node2 (active)", "Host node2 stays and follows this host"} {
		if !strings.Contains(out.String(), text) {
			t.Errorf("the output lacks %q:\n%s", text, out.String())
		}
	}
	current, err := raftnode.ReadState(files.RaftDir())
	if err != nil {
		t.Fatal(err)
	}
	if current.Hosts[1].Phase != state.Removed || current.Hosts[2].Phase != state.Active {
		t.Errorf("hosts %v, %v; want host 1 removed and the worker active", current.Hosts[1].Phase, current.Hosts[2].Phase)
	}

	// again, as after a failure: the worker rejoins again
	service, hosts.rejoined = nil, nil
	if err := Recover(context.Background(), files, recoverParts(driver, &service, hosts), options, logger); err != nil {
		t.Fatal(err)
	}
	if len(project.Changes()) != 2 || !slices.Equal(service, []string{"stop", "start"}) ||
		!slices.Equal(hosts.rejoined[2], []state.HostID{1}) {
		t.Errorf("a second recover: changes at Hetzner %q, service %q, rejoined %v", project.Changes(), service,
			hosts.rejoined)
	}
}

func TestRecoverStopsWhenAWorkerThatStaysFails(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	options := Options{Yes: true, Out: io.Discard}

	// before anything changed: the agent runs again
	files, project := lostClusterWith(t, hostrole.DefaultJoin)
	var service []string
	hosts := &fakeOtherHosts{answers: map[state.HostID]hostAnswer{2: {copiesErr: errors.New("lvs timed out")}}}
	err := Recover(context.Background(), files, recoverParts(&recordedStorage{}, &service, hosts), options, logger)
	if err == nil || !strings.Contains(err.Error(), "host node2 stays, but does not tell its copies") ||
		len(project.Changes()) != 0 || !slices.Equal(service, []string{"stop", "start"}) {
		t.Errorf("copies refused: %v, changes at Hetzner %q, service %q", err, project.Changes(), service)
	}

	// after the remap: the agent stays stopped, and a second run goes on
	files, _ = lostClusterWith(t, hostrole.DefaultJoin)
	service = nil
	hosts = &fakeOtherHosts{answers: map[state.HostID]hostAnswer{2: {rejoinErr: errors.New("this host knows a leader")}}}
	err = Recover(context.Background(), files, recoverParts(&recordedStorage{}, &service, hosts), options, logger)
	if err == nil || !strings.Contains(err.Error(), "tell host node2 to follow this host") ||
		!strings.Contains(err.Error(), "run recover again") || !slices.Equal(service, []string{"stop"}) {
		t.Errorf("rejoin refused: %v, service %q", err, service)
	}
	service, hosts.answers[2] = nil, hostAnswer{}
	if err := Recover(context.Background(), files, recoverParts(&recordedStorage{}, &service, hosts), options, logger); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(hosts.rejoined[2], []state.HostID{1}) || !slices.Equal(service, []string{"stop", "start"}) {
		t.Errorf("the second run: rejoined %v, service %q", hosts.rejoined, service)
	}
}

func TestRecoverCountsOnlyTheSyncedCopiesOfTheStatesGeneration(t *testing.T) {
	minor := 1
	volume := func(generation int64, copies ...state.VolumeCopy) *state.Volume {
		return &state.Volume{Minor: &minor, DataGeneration: generation, Copies: copies}
	}
	onTwo, tiebreakerOnTwo := state.VolumeCopy{Host: 2}, state.VolumeCopy{Host: 2, Tiebreaker: true}
	current := &state.State{Volumes: map[string]*state.Volume{
		"default/synced":     volume(3, onTwo),
		"default/older":      volume(2, onTwo),
		"default/outdated":   volume(1, onTwo),
		"default/tiebreaker": volume(1, tiebreakerOnTwo),
		"default/elsewhere":  volume(1, state.VolumeCopy{Host: 3}),
		"default/plain":      {DataGeneration: 1, Copies: []state.VolumeCopy{onTwo}},
	}}
	disks := map[string]string{
		names.VolumeName("default/synced", 3):     storage.DiskUpToDate,
		names.VolumeName("default/older", 1):      storage.DiskUpToDate,
		names.VolumeName("default/outdated", 1):   "Outdated",
		names.VolumeName("default/tiebreaker", 1): storage.DiskUpToDate,
		names.VolumeName("default/elsewhere", 1):  storage.DiskUpToDate,
		names.VolumeName("default/plain", 1):      storage.DiskUpToDate,
	}
	want := []state.SyncedCopy{{Volume: "default/synced", DataGeneration: 3, Host: 2}}
	if synced := syncedCopies(current, 2, disks); !slices.Equal(synced, want) {
		t.Errorf("synced %v, want %v", synced, want)
	}
}

func TestRecoverChangesNothingWhenItIsRefused(t *testing.T) {
	for _, test := range []struct {
		name, answer, want string
		answers            map[state.HostID]hostAnswer
	}{
		{"another host knows a leader", "node0\n", "host node2 knows a leader", map[state.HostID]hostAnswer{2: {knows: true}}},
		{
			"another host answers without saying", "node0\n", "host node1 answers, but not whether it knows a leader",
			map[state.HostID]hostAnswer{1: {err: errors.New("certificate refused")}},
		},
		{
			"a control-plane host answers", "node0\n", "host node1 is a control-plane host and answers",
			map[state.HostID]hostAnswer{1: {}},
		},
		{"the host's name is not typed", "prod\n", "not confirmed", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			files, project := lostCluster(t)
			driver := &recordedStorage{}
			var service []string
			parts := recoverParts(driver, &service, &fakeOtherHosts{answers: test.answers})
			options := Options{In: strings.NewReader(test.answer), Out: io.Discard}
			err := Recover(context.Background(), files, parts, options, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Errorf("got %v, want %q", err, test.want)
			}
			// the agent runs again, and nothing else changed
			if len(project.Changes()) != 0 || len(driver.dropped) != 0 || !slices.Equal(service, []string{"stop", "start"}) {
				t.Errorf("changes at Hetzner %q, dropped %v, service %q", project.Changes(), driver.dropped, service)
			}
			current, err := raftnode.ReadState(files.RaftDir())
			if err != nil || current.Hosts[1].Phase != state.Active {
				t.Errorf("the state changed: %v", err)
			}
		})
	}
}

func TestRecoverHasNothingToDoForASingleHostOrBeforeTheConfiguration(t *testing.T) {
	one := &state.State{Keys: &state.ClusterKeys{}, Hosts: map[state.HostID]*state.Host{0: {ID: 0, Phase: state.Active}}}
	if _, err := otherHosts(one, hostfiles.Host{ID: 0, Name: "node0"}); err == nil || !strings.Contains(err.Error(), "nothing to recover") {
		t.Errorf("a single host: %v", err)
	}
	one.RecoveredAt = time.Now()
	if others, err := otherHosts(one, hostfiles.Host{ID: 0, Name: "node0"}); err != nil || len(others) != 0 {
		t.Errorf("a single host after a recover: %v, %v", others, err)
	}
	one.Hosts[1] = &state.Host{ID: 1, Phase: state.Active}
	if _, err := otherHosts(one, hostfiles.Host{ID: 0, Name: "node0"}); err == nil || !strings.Contains(err.Error(), "no ClusterConfiguration names the Hetzner token") {
		t.Errorf("before the configuration: %v", err)
	}
}

func TestRecoverHasNothingToDoWhileEveryOtherHostAnswers(t *testing.T) {
	files, _ := lostClusterWith(t, hostrole.DefaultJoin)
	var service []string
	hosts := &fakeOtherHosts{answers: map[state.HostID]hostAnswer{2: {}}}
	parts := recoverParts(&recordedStorage{}, &service, hosts)
	current, err := raftnode.ReadState(files.RaftDir())
	if err != nil {
		t.Fatal(err)
	}
	r := recoverer{parts: parts, caller: hosts, host: hostfiles.Host{ID: 0, Name: "node0", APIPort: hostfiles.APIPort}}
	if _, err := r.ask(context.Background(), current, []*state.Host{current.Hosts[2]}); err == nil ||
		!strings.Contains(err.Error(), "nothing to recover") {
		t.Errorf("got %v, want nothing to recover", err)
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
			options := Options{Yes: true, Out: io.Discard}
			hosts := &fakeOtherHosts{}
			err := Recover(context.Background(), files, recoverParts(driver, &service, hosts), options, logger)
			if err == nil || !strings.Contains(err.Error(), "clowderd.service is stopped: run recover again") {
				t.Errorf("the first run: %v", err)
			}
			// it changed something: the agent stays stopped
			if !slices.Equal(service, []string{"stop"}) {
				t.Errorf("service %q", service)
			}
			service = nil
			if err := Recover(context.Background(), files, recoverParts(driver, &service, hosts), options, logger); err != nil {
				t.Fatal(err)
			}
			current, err := raftnode.ReadState(files.RaftDir())
			if err != nil || current.Hosts[2].Phase != state.Removed || !slices.Equal(service, []string{"stop", "start"}) {
				t.Errorf("after the second run: %v, service %q", err, service)
			}
		})
	}
}
