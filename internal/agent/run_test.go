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
	"testing"
	"time"

	"filippo.io/age"

	"github.com/webcodingcat/clowder/internal/cli"
	"github.com/webcodingcat/clowder/internal/clusterdir"
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

func noNetwork(context.Context, netip.Addr) error { return nil }

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
	err := Init(context.Background(), files, InitOptions{
		BootstrapKey: "the-bootstrap-key", Name: "node0", Underlay: loopback,
		NodeAddress: loopback, RaftPort: freePort(t), APIPort: apiPort,
	}, noNetwork, logger)
	if err != nil {
		t.Fatal(err)
	}
	if err := Init(context.Background(), files, InitOptions{BootstrapKey: "k", Name: "node0", Underlay: loopback},
		noNetwork, logger); err == nil || !strings.Contains(err.Error(), "was part of a cluster") {
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
	go func() { stopped <- Run(ctx, files, noNetwork, logger) }()
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
	if out, err := clowder(t, "--cluster", dir, "cluster", "diff"); err != nil || !strings.Contains(out, "network") {
		t.Errorf("diff after a change: %q, %v", out, err)
	}
	out, err = clowder(t, "--cluster", dir, "cluster", "status")
	if err != nil || !strings.Contains(out, "leader node0") || !strings.Contains(out, "revision 1") {
		t.Errorf("status: %q, %v", out, err)
	}
}
