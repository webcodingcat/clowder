package api

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/webcodingcat/clowder/internal/datakey"
	"github.com/webcodingcat/clowder/internal/hostrole"
	"github.com/webcodingcat/clowder/internal/pki"
	"github.com/webcodingcat/clowder/internal/state"
)

const testVersion = "1.4.0"

// fakeCluster is a Raft member without Raft: the leader applies commands
// at once.
type fakeCluster struct {
	mu       sync.Mutex
	state    *state.State
	leader   bool
	leaderID state.HostID
	// noLeader: an election runs, no leader is known
	noLeader bool
	changes  state.ChangeRing
	changed  chan struct{}
	// handOvers counts TransferLeadership
	handOvers int
}

// newFakeCluster is a member with a bootstrapped state, its ring complete
// from there.
func newFakeCluster(t *testing.T, leader bool) *fakeCluster {
	t.Helper()
	f := &fakeCluster{state: bootstrappedState(t), leader: leader, changed: make(chan struct{})}
	f.changes.Reset(f.state.Index)
	return f
}

func (f *fakeCluster) Propose(_ context.Context, cmd state.Command) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.leader {
		return 0, errors.New("not the leader")
	}
	index := f.state.Index + 1
	changes, err := f.state.Apply(index, cmd)
	f.changes.Add(changes)
	close(f.changed)
	f.changed = make(chan struct{})
	if err != nil {
		return index, &state.Refused{Err: err}
	}
	return index, nil
}

func (f *fakeCluster) Changes(since uint64) ([]state.Change, uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	changes, err := f.changes.Since(since)
	return changes, f.state.Index, err
}

func (f *fakeCluster) Changed() <-chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.changed
}

func (f *fakeCluster) Read(use func(current *state.State)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	use(f.state)
}

func (f *fakeCluster) WaitIndex(ctx context.Context, index uint64) error {
	for {
		f.mu.Lock()
		reached := f.state.Index >= index
		f.mu.Unlock()
		if reached {
			return nil
		}
		select {
		case <-time.After(10 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (f *fakeCluster) IsLeader() bool { return f.leader }

func (f *fakeCluster) Leader() (state.HostID, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.leaderID, !f.noLeader
}

// TransferLeadership counts the hand-overs and names host 1 the leader;
// this member still commits, as no other member would take the commands.
func (f *fakeCluster) TransferLeadership() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handOvers++
	f.leaderID = 1
	return nil
}

// setIndex moves the applied index, as replication would.
func (f *fakeCluster) setIndex(index uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state.Index = index
}

// testCluster is a CA, a bootstrapped state with hosts 0 and 1, and an API
// server per host 0, 1 and 2: host 0 leads, host 2 is a worker (rolesOf)
// that the state does not know unless a test adds it.
type testCluster struct {
	t        *testing.T
	ca       *pki.CA
	clusters []*fakeCluster
	servers  []*httptest.Server
	apis     []*Server
	// sealing is the public half of the cluster's sealing key: what the
	// values of SealedSecrets are sealed for
	sealing age.Recipient
	// joins answers the joins on every server; only the leader's is asked
	joins *fakeJoins
	// healths are the servers' beats, by host
	healths []*fakeHealth
	// volumes are the servers' volume copies, by host
	volumes []*fakeVolumes
	// recovers are what the servers do for clowderd recover, by host
	recovers []*fakeRecover
	// caughtUp closes once the agents caught up after their start
	caughtUp chan struct{}
}

func newTestCluster(t *testing.T) *testCluster {
	t.Helper()
	c := newCatchingUpCluster(t)
	close(c.caughtUp)
	return c
}

// newCatchingUpCluster is newTestCluster with agents that have not caught
// up after their start until caughtUp closes.
func newCatchingUpCluster(t *testing.T) *testCluster {
	t.Helper()
	ca, err := pki.NewCA("test", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	c := &testCluster{t: t, ca: ca, joins: &fakeJoins{}, caughtUp: make(chan struct{})}
	// a real data key: the state reads the encrypted length of secrets
	keys, err := datakey.New()
	if err != nil {
		t.Fatal(err)
	}
	// and a real sealing key, encrypted in the state as init does
	clusterIdentity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	c.sealing = clusterIdentity.Recipient()
	sealingKey, err := keys.Encrypt([]byte(clusterIdentity.String()))
	if err != nil {
		t.Fatal(err)
	}
	addresses := map[state.HostID]string{}
	for id := range state.HostID(3) {
		cluster := newFakeCluster(t, id == 0)
		cluster.state.Keys.SealingKey = sealingKey
		health := &fakeHealth{beats: map[state.HostID]int{}}
		c.healths = append(c.healths, health)
		volumes := &fakeVolumes{}
		c.volumes = append(c.volumes, volumes)
		recover := &fakeRecover{}
		c.recovers = append(c.recovers, recover)
		server := httptest.NewUnstartedServer(nil)
		api := NewServer(ServerConfig{
			Cluster: cluster, CA: ca.Certificate, Certificate: c.hostCertificate(id), Roles: rolesOf(id), HostID: id,
			APIAddress: func(id state.HostID) string { return addresses[id] },
			Version:    testVersion,
			Keys:       keys,
			Joins:      c.joins,
			Beats:      health,
			Volumes:    volumes,
			Recover:    recover,
			CaughtUp:   c.caughtUp,
			Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
		server.Config.Handler = api
		server.Config.ErrorLog = log.New(io.Discard, "", 0) // refused handshakes are part of the tests
		server.TLS = api.TLSConfig()
		server.StartTLS()
		t.Cleanup(server.Close)
		addresses[id] = server.Listener.Addr().String()
		c.clusters, c.servers, c.apis = append(c.clusters, cluster), append(c.servers, server), append(c.apis, api)
	}
	return c
}

func bootstrappedState(t *testing.T) *state.State {
	t.Helper()
	current := &state.State{}
	notAfter := time.Now().AddDate(1, 0, 0) // of the certificates
	apply := func(index uint64, kind string, payload any) {
		cmd, err := state.NewCommand(kind, time.Now(), state.LeaderActor, payload)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := current.Apply(index, cmd); err != nil {
			t.Fatal(err)
		}
	}
	apply(1, state.KindBootstrap, state.Bootstrap{
		Keys:        state.ClusterKeys{CACertificate: []byte("ca"), CAKey: state.Encrypted("key"), SealingKey: state.Encrypted("age")},
		ClusterName: "prod",
		Host: state.Host{
			ID: 0, Name: "node0", Roles: hostrole.DefaultInit, ServerID: 40, Underlay: netip.MustParseAddr("10.0.0.2"),
			PublicAddress: netip.MustParseAddr("203.0.113.2"), WGPublicKey: "wg", CertificateSerial: "s0",
			CertificateNotAfter: notAfter,
		},
	})
	// written in: the tests need host 1, not its join
	current.Hosts[1] = &state.Host{
		ID: 1, Name: "node1", Roles: hostrole.DefaultInit, ServerID: 41, Underlay: netip.MustParseAddr("10.0.0.3"),
		PublicAddress: netip.MustParseAddr("203.0.113.3"), WGPublicKey: "wg", CertificateSerial: "s1",
		CertificateNotAfter: notAfter, Phase: state.Joining,
	}
	return current
}

func (c *testCluster) certificate(issue func(public ed25519.PublicKey) ([]byte, error)) tls.Certificate {
	c.t.Helper()
	key, err := pki.NewKey()
	if err != nil {
		c.t.Fatal(err)
	}
	certificatePEM, err := issue(key.Public().(ed25519.PublicKey))
	if err != nil {
		c.t.Fatal(err)
	}
	leaf, err := pki.ParseCertificate(certificatePEM)
	if err != nil {
		c.t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{leaf.Raw}, PrivateKey: key, Leaf: leaf}
}

// workerHost is the test cluster's host without the control plane.
const workerHost state.HostID = 2

// rolesOf gives a host of the test cluster its roles: hosts 0 and 1 have
// every role, host 2 is a worker with ingress.
func rolesOf(id state.HostID) hostrole.Set {
	if id == workerHost {
		return hostrole.DefaultJoin
	}
	return hostrole.DefaultInit
}

func (c *testCluster) hostCertificate(id state.HostID) tls.Certificate {
	return c.certificate(func(public ed25519.PublicKey) ([]byte, error) {
		return c.ca.IssueHost(public, int(id), rolesOf(id), []netip.Addr{netip.MustParseAddr("127.0.0.1")}, time.Now())
	})
}

func (c *testCluster) cliCertificate() tls.Certificate {
	return c.certificate(func(public ed25519.PublicKey) ([]byte, error) {
		return c.ca.IssueCLI(public, "alice", time.Now())
	})
}

// client connects with a certificate to the given servers.
func (c *testCluster) client(certificate tls.Certificate, version string, ids ...int) *Client {
	var endpoints []string
	for _, id := range ids {
		endpoints = append(endpoints, c.servers[id].Listener.Addr().String())
	}
	return NewClient(endpoints, time.Second, certificate, c.ca.Certificate, version)
}

func statusOf(err error) int {
	var statusError *StatusError
	if errors.As(err, &statusError) {
		return statusError.Status
	}
	return 0
}

func TestTheCLIReadsTheStatusFromAnyAgent(t *testing.T) {
	cluster := newTestCluster(t)
	client := cluster.client(cluster.cliCertificate(), testVersion, 1)
	var status Status
	if err := client.Get(context.Background(), "/v1/status", &status); err != nil {
		t.Fatal(err)
	}
	if status.Leader != "node0" || len(status.Hosts) != 2 || status.Hosts[1].Phase != "joining" {
		t.Errorf("status %+v", status)
	}
}

func TestAFollowerForwardsWritesToTheLeader(t *testing.T) {
	cluster := newTestCluster(t)
	client := cluster.client(cluster.cliCertificate(), testVersion, 1)
	ctx := context.Background()

	// node0 is active: draining it is allowed
	written, err := client.Post(ctx, "/v1/hosts/0/drain", nil)
	if err != nil {
		t.Fatal(err)
	}
	var phase state.HostPhase
	var actor string
	cluster.clusters[0].Read(func(current *state.State) {
		phase = current.Hosts[0].Phase
		actor = current.Audit[len(current.Audit)-1].Actor
	})
	// the actor names the CLI certificate, also on the leader it was
	// forwarded to
	if phase != state.Draining || actor != "cli/alice" {
		t.Errorf("leader's host 0 is %s, last actor %s", phase, actor)
	}

	// the follower has not caught up: the next read waits for the write
	go func() {
		time.Sleep(100 * time.Millisecond)
		cluster.clusters[1].setIndex(written.Index)
	}()
	var hosts []HostInfo
	if err := client.Get(ctx, "/v1/hosts", &hosts); err != nil {
		t.Fatal(err)
	}
	cluster.clusters[1].Read(func(current *state.State) {
		if current.Index < written.Index {
			t.Errorf("the read did not wait for index %d", written.Index)
		}
	})

	// a refusal of the state comes back as such
	// the API checks the phase a user action starts from
	_, err = client.Post(ctx, "/v1/hosts/1/undrain", nil)
	if statusOf(err) != http.StatusConflict || !strings.Contains(err.Error(), "undrain works on a host that is draining") {
		t.Errorf("want 409 for undraining a joining host, got %v", err)
	}
	// the leader's state refuses what the API lets through: removing twice
	if _, err := client.Post(ctx, "/v1/hosts/1/remove", nil); err != nil {
		t.Fatal(err)
	}
	_, err = client.Post(ctx, "/v1/hosts/1/remove", nil)
	if statusOf(err) != http.StatusConflict || !strings.Contains(err.Error(), "node1 is removed") {
		t.Errorf("want 409 with the state's reason, got %v", err)
	}
	if _, err := client.Post(ctx, "/v1/hosts/1/explode", nil); statusOf(err) != http.StatusNotFound {
		t.Errorf("want 404 for an unknown action, got %v", err)
	}
}

func TestCallersAreCheckedOnEveryCall(t *testing.T) {
	cluster := newTestCluster(t)
	ctx := context.Background()
	revoked := cluster.hostCertificate(1)
	beforeCutoff := cluster.certificate(func(public ed25519.PublicKey) ([]byte, error) {
		return cluster.ca.IssueCLI(public, "alice", time.Now().Add(-time.Hour))
	})
	cluster.clusters[0].Read(func(current *state.State) {
		current.Revoked[pki.Serial(revoked.Leaf)] = state.Revocation{At: time.Now(), NotAfter: revoked.Leaf.NotAfter}
		current.CLICutoff = beforeCutoff.Leaf.NotBefore.Add(time.Minute)
	})
	tests := []struct {
		name   string
		client *Client
		want   int
	}{
		{"revoked host", cluster.client(revoked, testVersion, 0), http.StatusUnauthorized},
		{"CLI issued before the cutoff", cluster.client(beforeCutoff, testVersion, 0), http.StatusUnauthorized},
		{"host on a CLI route", cluster.client(cluster.hostCertificate(1), testVersion, 0), http.StatusForbidden},
		{"CLI two minor versions away", cluster.client(cluster.cliCertificate(), "1.2.0", 0), http.StatusBadRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.client.Get(ctx, "/v1/status", &Status{}); statusOf(err) != test.want {
				t.Errorf("want HTTP %d, got %v", test.want, err)
			}
		})
	}

	// without a certificate the TLS handshake works, the route does not
	plain := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		InsecureSkipVerify: true, // the test checks the server's refusal only
		MinVersion:         tls.VersionTLS13,
	}}}
	response, err := plain.Get(cluster.servers[0].URL + "/v1/status")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Errorf("without a certificate: HTTP %d", response.StatusCode)
	}

	// another cluster's CLI does not even get through the handshake
	stranger := newTestCluster(t).cliCertificate()
	if err := cluster.client(stranger, testVersion, 0).Get(ctx, "/v1/status", &Status{}); err == nil || statusOf(err) != 0 {
		t.Errorf("want a failed handshake, got %v", err)
	}
}

func TestInternalStackRoutesTakeOnlyStackNames(t *testing.T) {
	cluster := newTestCluster(t)
	host1 := cluster.client(cluster.hostCertificate(1), testVersion, 0)
	for _, route := range []string{"/v1/internal/stacks/Not_A_Stack/ps", "/v1/internal/stacks/Not_A_Stack/logs"} {
		if err := host1.Get(context.Background(), route, &[]ContainerInfo{}); statusOf(err) != http.StatusBadRequest {
			t.Errorf("%s: want HTTP 400, got %v", route, err)
		}
	}
}

func TestHostsProposeOnlyWhatTheyMay(t *testing.T) {
	cluster := newTestCluster(t)
	cluster.clusters[0].Read(func(current *state.State) {
		current.Hosts[workerHost] = &state.Host{ID: workerHost, Name: "node2", Roles: rolesOf(workerHost), Phase: state.Active}
	})
	// to the leader, as hosts forward with their own certificate
	host1 := cluster.client(cluster.hostCertificate(1), testVersion, 0)
	worker := cluster.client(cluster.hostCertificate(workerHost), testVersion, 0)
	tests := []struct {
		name    string
		from    *Client
		kind    string
		actor   string
		payload any
		allowed bool
	}{
		{
			"its own version", host1, state.KindReportHost, state.HostActor(1),
			state.ReportHost{Host: 1, Version: testVersion},
			true,
		},
		{
			"the CLI's write it forwards", host1, state.KindSetHostPhase, state.CLIActor("alice"),
			state.SetHostPhase{Host: 1, Phase: state.Active},
			true,
		},
		{
			"another host's version", host1, state.KindReportHost, state.HostActor(0),
			state.ReportHost{Host: 0, Version: "9.9.9"},
			false,
		},
		{
			"its own cutoff of the CLI certificates", host1, state.KindSetCLICutoff, state.HostActor(1),
			state.SetCLICutoff{Cutoff: time.Now()},
			true,
		},
		{
			"another host's cutoff", host1, state.KindSetCLICutoff, state.HostActor(0),
			state.SetCLICutoff{Cutoff: time.Now()},
			false,
		},
		{
			"a command as the leader", host1, state.KindAddJoinToken, state.LeaderActor,
			state.AddJoinToken{},
			false,
		},
		{
			"the CLI's write as itself", host1, state.KindAddJoinToken, state.HostActor(1),
			state.AddJoinToken{},
			false,
		},
		{
			"the leader's own work for the CLI", host1, state.KindSetIngress, state.CLIActor("alice"),
			state.SetIngress{Host: 0},
			false,
		},
		{
			"a worker's own version", worker, state.KindReportHost, state.HostActor(workerHost),
			state.ReportHost{Host: workerHost, Version: testVersion},
			true,
		},
		{
			"the CLI's write from a worker", worker, state.KindSetHostPhase, state.CLIActor("alice"),
			state.SetHostPhase{Host: 1, Phase: state.Active},
			false,
		},
		{
			"a worker's cutoff of the CLI certificates", worker, state.KindSetCLICutoff, state.HostActor(workerHost),
			state.SetCLICutoff{Cutoff: time.Now()},
			false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cmd, err := state.NewCommand(test.kind, time.Now(), test.actor, test.payload)
			if err != nil {
				t.Fatal(err)
			}
			_, err = test.from.Post(context.Background(), "/v1/internal/propose", cmd)
			switch {
			case test.allowed && err != nil:
				t.Errorf("refused: %v", err)
			case !test.allowed && statusOf(err) != http.StatusForbidden:
				t.Errorf("want 403, got %v", err)
			}
		})
	}
}

func TestTheClientTriesTheNextEndpoint(t *testing.T) {
	cluster := newTestCluster(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := listener.Addr().String()
	_ = listener.Close() // nobody listens here now
	endpoints := []string{dead, cluster.servers[0].Listener.Addr().String()}
	client := NewClient(endpoints, time.Second, cluster.cliCertificate(), cluster.ca.Certificate, testVersion)
	var status Status
	if err := client.Get(context.Background(), "/v1/status", &status); err != nil {
		t.Fatal(err)
	}
	if status.Leader != "node0" {
		t.Errorf("status %+v", status)
	}
	// a write that could not leave goes on to the next endpoint
	if _, err := client.Post(context.Background(), "/v1/hosts/0/drain", nil); err != nil {
		t.Errorf("a write after an unreachable endpoint: %v", err)
	}
}

// silentEndpoint takes connections and never answers, as a host whose network
// hangs; it counts the connections it took.
func silentEndpoint(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var taken atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		var held []net.Conn
		defer func() {
			for _, connection := range held {
				_ = connection.Close()
			}
		}()
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			taken.Add(1)
			held = append(held, connection)
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		<-done
	})
	return listener.Addr().String(), &taken
}

func TestTheClientGivesUpOnASilentEndpointAfterTheConnectTimeout(t *testing.T) {
	cluster := newTestCluster(t)
	silent, _ := silentEndpoint(t)
	endpoints := []string{silent, cluster.servers[0].Listener.Addr().String()}
	// a client each, so that each request meets the silent endpoint first
	newClient := func() *Client {
		return NewClient(endpoints, 200*time.Millisecond, cluster.cliCertificate(), cluster.ca.Certificate, testVersion)
	}
	started := time.Now()
	if err := newClient().Get(context.Background(), "/v1/status", &Status{}); err != nil {
		t.Errorf("a read after a silent endpoint: %v", err)
	}
	// a write that never got past the handshake goes on as well
	if _, err := newClient().Post(context.Background(), "/v1/hosts/0/drain", nil); err != nil {
		t.Errorf("a write after a silent endpoint: %v", err)
	}
	if err := newClient().Stream(context.Background(), "/v1/status", io.Discard); err != nil {
		t.Errorf("a stream after a silent endpoint: %v", err)
	}
	// generous for the race detector; the request's own timeout would be
	// 30 s each
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Errorf("three requests took %s", elapsed)
	}
}

func TestTheClientStartsWithTheEndpointThatAnsweredLast(t *testing.T) {
	cluster := newTestCluster(t)
	silent, taken := silentEndpoint(t)
	endpoints := []string{silent, cluster.servers[0].Listener.Addr().String()}
	client := NewClient(endpoints, 200*time.Millisecond, cluster.cliCertificate(), cluster.ca.Certificate, testVersion)
	for range 3 {
		if err := client.Get(context.Background(), "/v1/status", &Status{}); err != nil {
			t.Fatal(err)
		}
	}
	if got := taken.Load(); got != 1 {
		t.Errorf("the silent endpoint was tried %d times, want once", got)
	}
}

func TestTheHostsShowTheirAddresses(t *testing.T) {
	current := &state.State{Hosts: map[state.HostID]*state.Host{
		0: {ID: 0, Name: "node0", Underlay: netip.MustParseAddr("10.0.0.2"), PublicAddress: netip.MustParseAddr("203.0.113.10")},
	}}
	hosts := hostInfos(current)
	if hosts[0].Underlay != "10.0.0.2" || hosts[0].PublicAddress != "203.0.113.10" {
		t.Errorf("hosts %+v", hosts)
	}
}

func TestTheClientNamesAClusterOfAnotherCA(t *testing.T) {
	cluster, other := newTestCluster(t), newTestCluster(t)
	// credentials of the other cluster, against this one's agent
	client := NewClient([]string{cluster.servers[0].Listener.Addr().String()}, time.Second,
		other.cliCertificate(), other.ca.Certificate, testVersion)
	err := client.Get(context.Background(), "/v1/status", &Status{})
	if err == nil || !strings.Contains(err.Error(), "belongs to another cluster than your context") {
		t.Errorf("read: %v", err)
	}
	if err := client.Stream(context.Background(), "/v1/watch?kind=stack&allNamespaces=true", io.Discard); err == nil ||
		!strings.Contains(err.Error(), "belongs to another cluster") {
		t.Errorf("stream: %v", err)
	}
}

func TestARequestOverTheLimitIsNamedAsSuch(t *testing.T) {
	// a small limit in place of MaxRequestBytes (ServeHTTP's): reading 16 MiB
	// takes seconds with the race detector
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/apply", strings.NewReader(`{"resources": []}`))
	request.Body = http.MaxBytesReader(recorder, request.Body, 4)
	var decoded ApplyRequest
	writeError(recorder, http.StatusBadRequest, decodeStrict(request.Body, &decoded))
	if recorder.Code != http.StatusRequestEntityTooLarge || !strings.Contains(recorder.Body.String(), "larger than 16777216 bytes") {
		t.Errorf("answer %d: %s", recorder.Code, recorder.Body)
	}
}

func TestAWriteThatLeftIsNotSentAgain(t *testing.T) {
	cluster := newTestCluster(t)
	// this agent takes the request and hangs up without an answer
	hangsUp := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		connection, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = connection.Close()
		}
	}))
	hangsUp.TLS = &tls.Config{Certificates: []tls.Certificate{cluster.hostCertificate(1)}, MinVersion: tls.VersionTLS13}
	hangsUp.Config.ErrorLog = log.New(io.Discard, "", 0)
	hangsUp.StartTLS()
	defer hangsUp.Close()

	endpoints := []string{hangsUp.Listener.Addr().String(), cluster.servers[0].Listener.Addr().String()}
	client := NewClient(endpoints, time.Second, cluster.cliCertificate(), cluster.ca.Certificate, testVersion)
	_, err := client.Post(context.Background(), "/v1/hosts/0/drain", nil)
	if err == nil || !strings.Contains(err.Error(), "may have gone through") {
		t.Errorf("want a warning that the write may have gone through, got %v", err)
	}
	cluster.clusters[0].Read(func(current *state.State) {
		if current.Hosts[0].Phase != state.Active {
			t.Error("the write went to the second endpoint as well")
		}
	})
}

func TestAddressAddsTheAPIPort(t *testing.T) {
	for endpoint, want := range map[string]string{
		"10.0.0.2": "10.0.0.2:" + strconv.Itoa(Port), "node0.example.com": "node0.example.com:7443", "10.0.0.2:9000": "10.0.0.2:9000",
	} {
		if got := Address(endpoint); got != want {
			t.Errorf("%s: %s", endpoint, got)
		}
	}
}

func (f *fakeCluster) VerifyLeader(context.Context) error {
	if !f.leader {
		return errors.New("not the leader")
	}
	return nil
}

func (f *fakeCluster) CommitIndex() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state.Index
}

func (f *fakeCluster) WaitCommitted(ctx context.Context, index uint64) error {
	return f.WaitIndex(ctx, index)
}

// The CLI's call waits until the agent caught up after its start: its state
// may be empty until then. The hosts' own routes do not wait.
func TestTheCLIsCallWaitsUntilTheAgentCaughtUp(t *testing.T) {
	cluster := newCatchingUpCluster(t)
	client := cluster.client(cluster.cliCertificate(), testVersion, 0)
	done := make(chan error, 1)
	go func() {
		var status Status
		done <- client.Get(context.Background(), "/v1/status", &status)
	}()
	select {
	case err := <-done:
		t.Fatalf("answered before the catch-up: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(cluster.caughtUp)
	if err := <-done; err != nil {
		t.Errorf("after the catch-up: %v", err)
	}
}

func TestKnowsLeaderTellsWhoKnowsALeaderAndWhoIsNotReached(t *testing.T) {
	cluster := newTestCluster(t)
	ctx := context.Background()
	host := cluster.hostCertificate(1)
	ask := func(server int, certificate tls.Certificate) (bool, error) {
		caller := NewRecoverCaller(certificate, cluster.ca.Certificate, testVersion)
		return caller.KnowsLeader(ctx, cluster.servers[server].Listener.Addr().String())
	}
	// the leader, and a follower that hears it
	for server := range 2 {
		if knows, err := ask(server, host); err != nil || !knows {
			t.Errorf("host %d: %v, %v", server, knows, err)
		}
	}
	cluster.clusters[1].mu.Lock()
	cluster.clusters[1].noLeader = true
	cluster.clusters[1].mu.Unlock()
	if knows, err := ask(1, host); err != nil || knows {
		t.Errorf("a follower without a leader: %v, %v", knows, err)
	}
	// an answer that is no word on the leader leaves it unknown
	if _, err := ask(0, cluster.cliCertificate()); err == nil || errors.Is(err, ErrNotReached) {
		t.Errorf("the CLI's certificate: %v", err)
	}
	gone := cluster.servers[1].Listener.Addr().String()
	cluster.servers[1].Close()
	if _, err := NewRecoverCaller(host, cluster.ca.Certificate, testVersion).KnowsLeader(ctx, gone); !errors.Is(err, ErrNotReached) {
		t.Errorf("a host that is gone: %v", err)
	}
}
