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
	"strings"
	"sync"
	"testing"
	"time"

	"filippo.io/age"
	"go.yaml.in/yaml/v3"

	"github.com/webcodingcat/clowder/internal/api"
	"github.com/webcodingcat/clowder/internal/buildinfo"
	"github.com/webcodingcat/clowder/internal/clusterdir"
	"github.com/webcodingcat/clowder/internal/clusterspec"
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

// recoveryKeyFile is where startCluster puts the cluster's recovery key, in
// the test's home.
func recoveryKeyFile() string {
	return filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "recovery-key")
}

// startCluster runs an agent's API with one host and writes a cluster
// directory for alice that points at it.
func startCluster(t *testing.T) string {
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
		Keys:         state.ClusterKeys{CACertificate: ca.CertificatePEM(), CAKey: state.Sealed("key"), AgeIdentity: state.Sealed("age")},
		BootstrapKey: state.Sealed("join"),
		Host: state.Host{
			ID: 0, Name: "node0", Underlay: netip.MustParseAddr("10.0.0.2"), WGPublicKey: "wg", CertificateSerial: "s0",
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
		return ca.IssueHost(public, 0, []netip.Addr{netip.MustParseAddr("127.0.0.1")}, time.Now())
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
	// the cluster's own age key, sealed as init does, and a recovery key:
	// what secret encrypt encrypts for and the agent opens with
	clusterKey, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	if current.Keys.AgeIdentity, err = (ageKeys{dataKey}).Seal([]byte(clusterKey.String())); err != nil {
		t.Fatal(err)
	}
	// and the CA's key, with which the agent issues a recovered user's certificate
	caKey, err := ca.KeyPEM()
	if err != nil {
		t.Fatal(err)
	}
	if current.Keys.CAKey, err = (ageKeys{dataKey}).Seal(caKey); err != nil {
		t.Fatal(err)
	}
	recovery, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	host := &oneHost{state: current, changed: make(chan struct{})}
	host.changes.Reset(current.Index)
	agent := api.NewServer(api.ServerConfig{
		Cluster: host, CA: ca.Certificate, Certificate: serverCertificate,
		APIAddress: func(state.HostID) string { return server.Listener.Addr().String() },
		Version:    buildinfo.Version, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Alerts: fakeAlerts{}, Keys: ageKeys{dataKey},
	})
	server.Config.Handler = agent
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.TLS = agent.TLSConfig()
	server.StartTLS()
	t.Cleanup(server.Close)

	dir := t.TempDir()
	// no secret in it: these commands need only the cluster's name and endpoints
	config := clusterspec.Config{
		Name: "test", Endpoints: []string{server.Listener.Addr().String()},
		Ingress:        clusterspec.Ingress{FloatingIP: "203.0.113.11"},
		StorageClasses: map[string]clusterspec.StorageClass{"fast": {Pool: "vg-fast/thin"}},
	}
	writeYAML(t, filepath.Join(dir, clusterdir.ConfigFile), config)

	userCertificate, userKey := issue(func(public ed25519.PublicKey) ([]byte, error) {
		return ca.IssueUser(public, "alice", time.Now())
	})
	parsed, err := pki.ParseCertificate(userCertificate)
	if err != nil {
		t.Fatal(err)
	}
	addAlice, err := state.NewCommand(state.KindAddUser, time.Now(), state.LeaderActor,
		state.AddUser{
			Name: "alice", Serial: pki.Serial(parsed), NotAfter: parsed.NotAfter,
			RecoveryRecipient: recovery.Recipient().String(),
		})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := host.Propose(context.Background(), addAlice); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir() // no credentials of the developer join in
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("AppData", home)
	if err := os.WriteFile(recoveryKeyFile(), []byte(recovery.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = clusterdir.WriteCredentials(config.Name, clusterdir.Credentials{
		User: "alice", CA: string(ca.CertificatePEM()), Certificate: string(userCertificate), Key: string(userKey),
	})
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// ageKeys seal as the agent's data key does (the CLI may not import
// datakey): the state reads the length of what age seals.
type ageKeys struct{ identity *age.X25519Identity }

func (k ageKeys) Seal(plaintext []byte) (state.Sealed, error) {
	var sealed bytes.Buffer
	writer, err := age.Encrypt(&sealed, k.identity.Recipient())
	if err != nil {
		return nil, err
	}
	if _, err := writer.Write(plaintext); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return sealed.Bytes(), nil
}

func (k ageKeys) Open(sealed state.Sealed) ([]byte, error) {
	reader, err := age.Decrypt(bytes.NewReader(sealed), k.identity)
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
	dir := startCluster(t)
	out, err := run(t, "--cluster", dir, "cluster", "status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no open alert") || !strings.Contains(out, "pool-use/node0/vg/thin/data") {
		t.Errorf("cluster status:\n%s", out)
	}
	out, err = run(t, "--cluster", dir, "alert", "test")
	if err == nil || !strings.Contains(err.Error(), "1 of 2 channels") ||
		!strings.Contains(out, "alerts.channels[0] (discord): sent") || !strings.Contains(out, "failed: no STARTTLS") {
		t.Errorf("alert test: %v\n%s", err, out)
	}
}

func writeYAML(t *testing.T, path string, value any) {
	t.Helper()
	encoded, err := yaml.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
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
	dir := startCluster(t)
	_, err := run(t, "--cluster", dir, "--connect-timeout", "0s", "host", "list")
	if ExitCode(err) != 2 || !strings.Contains(err.Error(), "--connect-timeout") {
		t.Errorf("want a wrong call that names --connect-timeout, got %v", err)
	}
	if _, err := run(t, "--cluster", dir, "--connect-timeout", "2s", "host", "list"); err != nil {
		t.Errorf("with --connect-timeout 2s: %v", err)
	}
}

func TestHostListShowsThePublicAddressOrADash(t *testing.T) {
	var out bytes.Buffer
	hosts := []api.HostInfo{
		{ID: 0, Name: "node0", Phase: "active", Underlay: "10.0.0.2", PublicAddress: "203.0.113.10"},
		{ID: 1, Name: "node1", Phase: "active", Underlay: "198.51.100.3"},
	}
	if err := printHosts(&out, hosts); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 || strings.Fields(lines[1])[5] != "203.0.113.10" || strings.Fields(lines[2])[5] != "-" {
		t.Errorf("host list:\n%s", out.String())
	}
}

func TestHostCommandsTalkToTheCluster(t *testing.T) {
	dir := startCluster(t)
	out, err := run(t, "--cluster", dir, "host", "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "node0") || !strings.Contains(out, "active") || !strings.Contains(out, "PUBLIC-ADDRESS") {
		t.Errorf("host list:\n%s", out)
	}

	if _, err := run(t, "--cluster", dir, "host", "drain", "node0"); err != nil {
		t.Fatal(err)
	}
	out, err = run(t, "--cluster", dir, "cluster", "status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "leader node0") || !strings.Contains(out, "draining") {
		t.Errorf("cluster status:\n%s", out)
	}

	if _, err := run(t, "--cluster", dir, "host", "drain", "node0"); err == nil ||
		!strings.Contains(err.Error(), "drain works on a host that is active") {
		t.Errorf("want the state's refusal, got %v", err)
	}
	if _, err := run(t, "--cluster", dir, "host", "drain", "node9"); err == nil || !strings.Contains(err.Error(), `no host "node9"`) {
		t.Errorf("want an unknown host, got %v", err)
	}
}

func TestRemovingAHostNeedsAConfirmation(t *testing.T) {
	dir := startCluster(t)
	// the test's stdin is no terminal, so only --yes confirms
	if _, err := run(t, "--cluster", dir, "host", "remove", "node0"); err == nil ||
		!strings.Contains(err.Error(), "confirm it with --yes") {
		t.Errorf("want a refusal without --yes, got %v", err)
	}
	if _, err := run(t, "--cluster", dir, "host", "remove", "node0", "--yes"); err == nil ||
		!strings.Contains(err.Error(), "the last that runs stacks") {
		t.Errorf("want the state's refusal for the only host, got %v", err)
	}
}
