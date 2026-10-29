package agent

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/webcodingcat/clowder/internal/cli"
	"github.com/webcodingcat/clowder/internal/clusterdir"
	"github.com/webcodingcat/clowder/internal/network"
	"github.com/webcodingcat/clowder/internal/sopsage"
)

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

// fakeNetwork stands in for the host's network and remembers the rules.
type fakeNetwork struct {
	mu       sync.Mutex
	rulesets []network.Ruleset
}

func (f *fakeNetwork) SetUpInterface(context.Context, netip.Addr) error { return nil }

func (f *fakeNetwork) ApplyRules(_ context.Context, ruleset network.Ruleset) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rulesets = append(f.rulesets, ruleset)
	return nil
}

func (f *fakeNetwork) StartResolver() (io.Closer, error) { return io.NopCloser(nil), nil }

// waitForRuleset waits up to 10 s until the last applied ruleset satisfies
// want.
func (f *fakeNetwork) waitForRuleset(t *testing.T, want func(network.Ruleset) bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		found := len(f.rulesets) > 0 && want(f.rulesets[len(f.rulesets)-1])
		f.mu.Unlock()
		if found {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("no such ruleset applied; the last ones: %v", f.rulesets)
}

// clowder runs the CLI and returns what it printed.
func clowder(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	root := cli.NewRoot()
	root.SetOut(&out)
	root.SetArgs(args)
	err := root.ExecuteContext(context.Background())
	return out.String(), err
}

// TestAFirstHostAndItsFirstUser goes the way of a new cluster: init on the
// host, the agent running, the user's bootstrap, apply and diff.
func TestAFirstHostAndItsFirstUser(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	loopback := netip.MustParseAddr("127.0.0.1")
	files := Files{Dir: t.TempDir()}
	apiPort := freePort(t)
	hostNetwork := &fakeNetwork{}
	err := Init(context.Background(), files, InitOptions{
		BootstrapKey: "the-bootstrap-key", Name: "node0", Underlay: loopback,
		NodeAddress: loopback, RaftPort: freePort(t), APIPort: apiPort,
	}, hostNetwork, logger)
	if err != nil {
		t.Fatal(err)
	}
	if err := Init(context.Background(), files, InitOptions{BootstrapKey: "k", Name: "node0", Underlay: loopback},
		hostNetwork, logger); err == nil || !strings.Contains(err.Error(), "was part of a cluster") {
		t.Errorf("a second init: %v", err)
	}

	// systemd's notify socket, to see the agent report READY=1
	socketPath := filepath.Join(t.TempDir(), "notify")
	systemd, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: socketPath, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = systemd.Close() }()
	t.Setenv("NOTIFY_SOCKET", socketPath)

	ctx, stop := context.WithCancel(context.Background())
	stopped := make(chan error, 1)
	go func() { stopped <- Run(ctx, files, hostNetwork, logger) }()
	defer func() {
		stop()
		if err := <-stopped; err != nil {
			t.Errorf("the agent stopped with %v", err)
		}
	}()
	message := make([]byte, 64)
	_ = systemd.SetReadDeadline(time.Now().Add(30 * time.Second))
	if n, err := systemd.Read(message); err != nil || string(message[:n]) != "READY=1" {
		t.Fatalf("systemd heard %q, %v", message[:n], err)
	}

	// the user's side: an age key, an empty cluster directory
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("SOPS_AGE_KEY", identity.String())
	t.Setenv("SOPS_AGE_KEY_FILE", "")
	t.Setenv(clusterdir.EnvVar, "")
	t.Setenv(clusterdir.UserEnvVar, "")
	dir := t.TempDir()
	seed := net.JoinHostPort("127.0.0.1", strconv.Itoa(apiPort))
	bootstrap := []string{
		"--cluster", dir, "cluster", "bootstrap", "alice", "--age", identity.Recipient().String(),
		"--seed", seed,
	}

	if _, err := clowder(t, append(bootstrap, "--key", "a-guess")...); err == nil ||
		!strings.Contains(err.Error(), "cannot prove it knows the bootstrap key") {
		t.Errorf("a wrong bootstrap key: %v", err)
	}
	if _, err := clowder(t, append(bootstrap, "--key", "the-bootstrap-key")...); err != nil {
		t.Fatal(err)
	}
	if _, err := clowder(t, "--cluster", t.TempDir(), "cluster", "bootstrap", "bob",
		"--age", identity.Recipient().String(), "--seed", seed, "--key", "the-bootstrap-key"); err == nil ||
		!strings.Contains(err.Error(), "first user already") {
		t.Errorf("a second bootstrap: %v", err)
	}

	out, err := clowder(t, "--cluster", dir, "cluster", "apply")
	if err != nil || !strings.Contains(out, "revision 1") {
		t.Fatalf("apply: %q, %v", out, err)
	}
	// the Floating IP of cluster.yaml reaches the nft rules (HTTP hairpin)
	hostNetwork.waitForRuleset(t, func(ruleset network.Ruleset) bool {
		return ruleset.FloatingIP == netip.MustParseAddr("203.0.113.10")
	})
	if out, err := clowder(t, "--cluster", dir, "cluster", "diff"); err != nil || !strings.Contains(out, "nothing to apply") {
		t.Errorf("diff after apply: %q, %v", out, err)
	}
	config := filepath.Join(dir, clusterdir.ConfigFile)
	content, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, append(content, []byte("network: public\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := clowder(t, "--cluster", dir, "cluster", "diff"); err != nil || !strings.Contains(out, "network") ||
		strings.Contains(out, "secrets") {
		t.Errorf("diff after a change of cluster.yaml: %q, %v", out, err)
	}
	// and now the hoster token as well: diff names both
	secrets, err := sopsage.Encrypt([]byte("secrets: {hcloud-token: new}\njoin_keys: [the-bootstrap-key]\n"),
		[]string{identity.Recipient().String()}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, clusterdir.SecretsFile), secrets, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := clowder(t, "--cluster", dir, "cluster", "diff"); err != nil || !strings.Contains(out, "network") ||
		!strings.Contains(out, "secrets differ") {
		t.Errorf("diff after both changed: %q, %v", out, err)
	}
	out, err = clowder(t, "--cluster", dir, "cluster", "status")
	if err != nil || !strings.Contains(out, "leader node0") || !strings.Contains(out, "revision 1") {
		t.Errorf("status: %q, %v", out, err)
	}
}
