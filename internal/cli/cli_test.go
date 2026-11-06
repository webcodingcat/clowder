package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"log/slog"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/webcodingcat/clowder/internal/api"
	"github.com/webcodingcat/clowder/internal/buildinfo"
	"github.com/webcodingcat/clowder/internal/clientconfig"
	"github.com/webcodingcat/clowder/internal/clusterspec"
	"github.com/webcodingcat/clowder/internal/hostrole"
	"github.com/webcodingcat/clowder/internal/pki"
	"github.com/webcodingcat/clowder/internal/state"
)

// oneHost is a leader that applies commands at once.
type oneHost struct {
	mu      sync.Mutex
	state   *state.State
	changes state.ChangeRing
	changed chan struct{}
}

func (h *oneHost) Propose(_ context.Context, cmd state.Command) (uint64, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	index := h.state.Index + 1
	changes, err := h.state.Apply(index, cmd)
	h.changes.Add(changes)
	close(h.changed)
	h.changed = make(chan struct{})
	if err != nil {
		return index, &state.Refused{Err: err}
	}
	return index, nil
}

func (h *oneHost) Changes(since uint64) ([]state.Change, uint64, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	changes, err := h.changes.Since(since)
	return changes, h.state.Index, err
}

func (h *oneHost) Changed() <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.changed
}

func (h *oneHost) Read(use func(current *state.State)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	use(h.state)
}

func (h *oneHost) WaitIndex(context.Context, uint64) error     { return nil }
func (h *oneHost) IsLeader() bool                              { return true }
func (h *oneHost) Leader() (state.HostID, bool)                { return 0, true }
func (h *oneHost) TransferLeadership() error                   { return errors.New("one host has no other voter") }
func (h *oneHost) VerifyLeader(context.Context) error          { return nil }
func (h *oneHost) CommitIndex() uint64                         { return 0 }
func (h *oneHost) WaitCommitted(context.Context, uint64) error { return nil }

// sealingKeyFile is where startCluster puts the cluster's sealing key, in
// the test's home.
func sealingKeyFile() string {
	return filepath.Join(os.Getenv("HOME"), "sealing-key")
}

// publicSealingKeyFile is where startCluster puts the public half of the
// sealing key, for --cert.
func publicSealingKeyFile() string {
	return filepath.Join(os.Getenv("HOME"), "sealing-key.pub")
}

// testServers are the servers the fake Hetzner token of startCluster sees:
// node0's.
var testServers = []int64{40}

// startCluster runs an agent's API with one host and writes alice's
// credentials for it into the test's home.
func startCluster(t *testing.T) {
	t.Helper()
	ca, err := pki.NewCA("test", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	issue := func(sign func(public ed25519.PublicKey) ([]byte, error)) ([]byte, []byte) {
		key, err := pki.NewKey()
		if err != nil {
			t.Fatal(err)
		}
		certificatePEM, err := sign(key.Public().(ed25519.PublicKey))
		if err != nil {
			t.Fatal(err)
		}
		keyPEM, err := pki.EncodeKey(key)
		if err != nil {
			t.Fatal(err)
		}
		return certificatePEM, keyPEM
	}

	current := &state.State{}
	cmd, err := state.NewCommand(state.KindBootstrap, time.Now(), state.LeaderActor, state.Bootstrap{
		Keys:        state.ClusterKeys{CACertificate: ca.CertificatePEM(), CAKey: state.Encrypted("key"), SealingKey: state.Encrypted("age")},
		ClusterName: "test",
		Host: state.Host{
			ID: 0, Name: "node0", Roles: hostrole.DefaultInit, ServerID: 40, Underlay: netip.MustParseAddr("10.0.0.2"),
			PublicAddress: netip.MustParseAddr("203.0.113.2"), WGPublicKey: "wg", CertificateSerial: "s0",
			CertificateNotAfter: time.Now().AddDate(1, 0, 0),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := current.Apply(1, cmd); err != nil {
		t.Fatal(err)
	}
	current.Config.Spec.StorageClasses = map[string]clusterspec.StorageClass{"fast": {Pool: "vg-fast/thin"}}
	hostCertificate, hostKey := issue(func(public ed25519.PublicKey) ([]byte, error) {
		return ca.IssueHost(public, 0, hostrole.DefaultInit, []netip.Addr{netip.MustParseAddr("127.0.0.1")}, time.Now())
	})
	serverCertificate, err := tls.X509KeyPair(hostCertificate, hostKey)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(nil)
	dataKey, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	// the cluster's sealing key, encrypted with the data key as init does:
	// what secret seal seals for and the agent opens with
	sealingKey, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	if current.Keys.SealingKey, err = (ageKeys{dataKey}).Encrypt([]byte(sealingKey.String())); err != nil {
		t.Fatal(err)
	}
	// and the CA's key, with which the agent issues a recovered user's certificate
	caKey, err := ca.KeyPEM()
	if err != nil {
		t.Fatal(err)
	}
	if current.Keys.CAKey, err = (ageKeys{dataKey}).Encrypt(caKey); err != nil {
		t.Fatal(err)
	}
	host := &oneHost{state: current, changed: make(chan struct{})}
	host.changes.Reset(current.Index)
	agentHost := api.HostConfig{
		Cluster: host, CA: ca.Certificate, Certificate: serverCertificate, Roles: hostrole.DefaultInit,
		APIAddress: func(state.HostID) string { return server.Listener.Addr().String() },
		Version:    buildinfo.Version, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Keys: ageKeys{dataKey},
	}
	agent := api.NewServer(api.ServerConfig{
		HostConfig: agentHost, Peers: api.NewPeers(agentHost), Alerts: fakeAlerts{},
		HetznerServers: func(context.Context, string, string) ([]int64, error) { return testServers, nil },
	})
	server.Config.Handler = agent
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.TLS = agent.TLSConfig()
	server.StartTLS()
	t.Cleanup(server.Close)

	cliCertificate, cliKey := issue(func(public ed25519.PublicKey) ([]byte, error) {
		return ca.IssueCLI(public, "alice", time.Now())
	})
	home := t.TempDir() // no credentials of the developer join in
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // the home on Windows
	t.Setenv(clientconfig.EnvVar, "")
	// as clowderd sealing-key export and export --public write them
	if err := os.WriteFile(sealingKeyFile(), []byte(sealingKey.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(publicSealingKeyFile(), []byte(sealingKey.Recipient().String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var credentials clientconfig.Config
	err = credentials.Add(clientconfig.Context{
		Name: "test", Endpoints: []string{server.Listener.Addr().String()},
		CA: string(ca.CertificatePEM()), Certificate: string(cliCertificate), Key: string(cliKey),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := clientconfig.Write(filepath.Join(home, ".clowder", "config"), credentials); err != nil {
		t.Fatal(err)
	}
}

// ageKeys encrypt as the agent's data key does (the CLI may not import
// datakey): the state reads the length of what age encrypts.
type ageKeys struct{ identity *age.X25519Identity }

func (k ageKeys) Encrypt(plaintext []byte) (state.Encrypted, error) {
	var encrypted bytes.Buffer
	writer, err := age.Encrypt(&encrypted, k.identity.Recipient())
	if err != nil {
		return nil, err
	}
	if _, err := writer.Write(plaintext); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return encrypted.Bytes(), nil
}

func (k ageKeys) Decrypt(encrypted state.Encrypted) ([]byte, error) {
	reader, err := age.Decrypt(bytes.NewReader(encrypted), k.identity)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(reader)
}

func (ageKeys) Derive(purpose string) ([]byte, error) { return []byte(purpose), nil }

// fakeAlerts has two channels, the second of which fails, and one message
// the email channel has not taken.
type fakeAlerts struct{}

func (fakeAlerts) Test(context.Context) ([]api.AlertTestResult, error) {
	return []api.AlertTestResult{
		{Channel: "alerts.channels[0] (discord)"}, {Channel: "alerts.channels[1] (email)", Error: "no STARTTLS"},
	}, nil
}

func (fakeAlerts) Failures() []api.AlertFailure {
	return []api.AlertFailure{{
		Channel: "alerts.channels[1] (email)", Key: "pool-use/node0/vg/thin/data", Event: "raised", Attempts: 2,
		LastError: "no STARTTLS",
	}}
}

func TestAlertsShowInTheStatusAndTheTestNamesEachChannel(t *testing.T) {
	startCluster(t)
	out, err := run(t, "cluster", "status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no open alert") || !strings.Contains(out, "pool-use/node0/vg/thin/data") {
		t.Errorf("cluster status:\n%s", out)
	}
	out, err = run(t, "alert", "test")
	if err == nil || !strings.Contains(err.Error(), "1 of 2 channels") ||
		!strings.Contains(out, "alerts.channels[0] (discord): sent") || !strings.Contains(out, "failed: no STARTTLS") {
		t.Errorf("alert test: %v\n%s", err, out)
	}
}

// run runs the CLI and returns what it printed.
func run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	root := NewRoot()
	root.SetOut(&out)
	root.SetArgs(args)
	err := root.ExecuteContext(context.Background())
	return out.String(), err
}

func TestTheConnectTimeoutMustBePositive(t *testing.T) {
	startCluster(t)
	_, err := run(t, "--connect-timeout", "0s", "host", "list")
	if ExitCode(err) != 2 || !strings.Contains(err.Error(), "--connect-timeout") {
		t.Errorf("want a wrong call that names --connect-timeout, got %v", err)
	}
	if _, err := run(t, "--connect-timeout", "2s", "host", "list"); err != nil {
		t.Errorf("with --connect-timeout 2s: %v", err)
	}
}

func TestHostListShowsTheRolesAndAddresses(t *testing.T) {
	var out bytes.Buffer
	hosts := []api.HostInfo{
		{
			ID: 0, Name: "node0", Roles: hostrole.DefaultJoin, Phase: "active", Underlay: "10.0.0.2",
			PublicAddress: "203.0.113.10",
		},
	}
	if err := printHosts(&out, hosts); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	want := [][]string{
		{"ID", "NAME", "ROLES", "PHASE", "VOTER", "UNDERLAY", "PUBLIC-ADDRESS", "VERSION"},
		{"0", "node0", "worker,ingress", "active", "no", "10.0.0.2", "203.0.113.10"},
	}
	if len(lines) != len(want) || !slices.Equal(strings.Fields(lines[0]), want[0]) ||
		!slices.Equal(strings.Fields(lines[1]), want[1]) {
		t.Errorf("host list:\n%s", out.String())
	}
}

func TestAJoinTokenAllowsTheRolesAsked(t *testing.T) {
	startCluster(t)
	if _, err := run(t, "host", "join-token", "--roles", "worker,database"); err == nil ||
		!strings.Contains(err.Error(), `--roles: unknown role "database"`) {
		t.Errorf("a token with an unknown role: %v", err)
	}
	if out, err := run(t, "host", "join-token", "--roles", "control-plane"); err != nil || !strings.Contains(out, ".") {
		t.Errorf("a token for a control-plane host: %q, %v", out, err)
	}
}

func TestHostCommandsTalkToTheCluster(t *testing.T) {
	startCluster(t)
	out, err := run(t, "host", "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "node0") || !strings.Contains(out, "control-plane,worker,ingress") ||
		!strings.Contains(out, "active") || !strings.Contains(out, "PUBLIC-ADDRESS") {
		t.Errorf("host list:\n%s", out)
	}

	if _, err := run(t, "host", "drain", "node0"); err != nil {
		t.Fatal(err)
	}
	out, err = run(t, "cluster", "status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "leader node0") || !strings.Contains(out, "draining") ||
		!strings.Contains(out, "cluster test, no configuration,") {
		t.Errorf("cluster status:\n%s", out)
	}

	if _, err := run(t, "host", "drain", "node0"); err == nil ||
		!strings.Contains(err.Error(), "drain works on a host that is active") {
		t.Errorf("want the state's refusal, got %v", err)
	}
	if _, err := run(t, "host", "drain", "node9"); err == nil || !strings.Contains(err.Error(), `no host "node9"`) {
		t.Errorf("want an unknown host, got %v", err)
	}
}

func TestRemovingAHostNeedsAConfirmation(t *testing.T) {
	startCluster(t)
	// the test's stdin is no terminal, so only --yes confirms
	if _, err := run(t, "host", "remove", "node0"); err == nil ||
		!strings.Contains(err.Error(), "confirm it with --yes") {
		t.Errorf("want a refusal without --yes, got %v", err)
	}
	if _, err := run(t, "host", "remove", "node0", "--yes"); err == nil ||
		!strings.Contains(err.Error(), "the last control-plane host in service") {
		t.Errorf("want the state's refusal for the only host, got %v", err)
	}
}
