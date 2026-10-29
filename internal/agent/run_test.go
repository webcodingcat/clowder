package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/webcodingcat/clowder/internal/api"
	"github.com/webcodingcat/clowder/internal/cli"
	"github.com/webcodingcat/clowder/internal/clusterdir"
	"github.com/webcodingcat/clowder/internal/compose"
	"github.com/webcodingcat/clowder/internal/docker"
	"github.com/webcodingcat/clowder/internal/network"
	"github.com/webcodingcat/clowder/internal/sopsage"
	"github.com/webcodingcat/clowder/internal/storage"
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

// calls is what a fake was asked to do, one line per call.
type calls struct {
	mu    sync.Mutex
	lines []string
}

func (c *calls) add(format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, fmt.Sprintf(format, args...))
}

// waitFor waits up to 10 s for a call.
func (c *calls) waitFor(t *testing.T, line string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		found := slices.Contains(c.lines, line)
		c.mu.Unlock()
		if found {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	t.Errorf("no call %q; the calls: %q", line, c.lines)
}

// fakeStorage stands in for the storage driver: every pool has 100 GiB.
type fakeStorage struct {
	calls
	dir string
}

func (f *fakeStorage) Ensure(_ context.Context, volume storage.Volume) error {
	f.add("ensure %s", volume.LV)
	return nil
}

func (f *fakeStorage) Remove(_ context.Context, volume storage.Volume) error {
	f.add("remove %s", volume.LV)
	return nil
}

func (f *fakeStorage) Trim(context.Context, []storage.Volume) error { return nil }

func (f *fakeStorage) PoolUsage(context.Context, string) (int64, float64, float64, error) {
	return 100 << 30, 0, 0, nil
}

func (f *fakeStorage) MountPoint(volume storage.Volume) string {
	return filepath.Join(f.dir, filepath.FromSlash(volume.ID))
}

// fakeDocker stands in for Docker: it sends no events and has one
// container per stack.
type fakeDocker struct {
	calls
	// failedUps is how many compose ups fail before one works
	failedUps int
	// upFiles are the files of the last compose up
	upFiles []string
}

func (f *fakeDocker) Events(ctx context.Context, _ string) (<-chan docker.Event, <-chan error) {
	events := make(chan docker.Event)
	go func() {
		<-ctx.Done()
		close(events)
	}()
	return events, make(chan error)
}

func (f *fakeDocker) Start(context.Context, string) error   { return nil }
func (f *fakeDocker) Restart(context.Context, string) error { return nil }

func (f *fakeDocker) ComposeUp(_ context.Context, stack string, files []string) error {
	f.add("up %s", stack)
	f.mu.Lock()
	f.upFiles = files
	f.mu.Unlock()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failedUps > 0 {
		f.failedUps--
		return errors.New("pull access denied")
	}
	return nil
}

func (f *fakeDocker) ComposeDown(_ context.Context, stack, _ string) error {
	f.add("down %s", stack)
	return nil
}

func (f *fakeDocker) ComposePs(context.Context, string, string) (string, error) {
	return `{"Service":"app","State":"running","Health":"","Status":"Up 2 seconds"}` + "\n", nil
}

func (f *fakeDocker) ComposeLogs(_ context.Context, _, _ string, _ bool, out io.Writer) error {
	_, err := fmt.Fprintln(out, "app-1  | hello")
	return err
}

func (f *fakeDocker) Login(context.Context, string, string, string) error { return nil }

// keyFile writes a bootstrap key into a file of its own, as the user keeps it.
func keyFile(t *testing.T, key string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bootstrap-key")
	if err := os.WriteFile(path, []byte(key), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// waitForOutput runs the CLI up to 10 s until its output contains want.
func waitForOutput(t *testing.T, want string, args ...string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var out string
	var err error
	for time.Now().Before(deadline) {
		if out, err = clowder(t, args...); err == nil && strings.Contains(out, want) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("clowder %s: no %q in %q, %v", strings.Join(args, " "), want, out, err)
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
	files := Files{Dir: t.TempDir(), SecretsDir: t.TempDir()}
	apiPort := freePort(t)
	hostNetwork := &fakeNetwork{}
	// the public address is another loopback address, as a host's public
	// address is another than its underlay; users reach the API there
	err := Init(context.Background(), files, InitOptions{
		BootstrapKey: "the-bootstrap-key", Name: "node0", Underlay: loopback,
		PublicAddress: netip.MustParseAddr("127.0.0.2"), ServerRef: "1000001",
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
	hostStorage := &fakeStorage{dir: t.TempDir()}
	hostDocker := &fakeDocker{}
	parts := HostParts{Network: hostNetwork, Storage: hostStorage, Docker: hostDocker}
	go func() { stopped <- Run(ctx, files, parts, logger) }()
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
	seed := net.JoinHostPort("127.0.0.2", strconv.Itoa(apiPort))
	bootstrap := []string{
		"--cluster", dir, "cluster", "bootstrap", "alice", "--age", identity.Recipient().String(),
		"--seed", seed,
	}

	if _, err := clowder(t, append(bootstrap, "--key-file", keyFile(t, "a-guess"))...); err == nil ||
		!strings.Contains(err.Error(), "cannot prove it knows the bootstrap key") {
		t.Errorf("a wrong bootstrap key: %v", err)
	}
	theKey := keyFile(t, "the-bootstrap-key\n")
	if _, err := clowder(t, append(bootstrap, "--key-file", theKey)...); err != nil {
		t.Fatal(err)
	}
	if _, err := clowder(t, "--cluster", t.TempDir(), "cluster", "bootstrap", "bob",
		"--age", identity.Recipient().String(), "--seed", seed, "--key-file", theKey); err == nil ||
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

	// a stack: created with its directory, then with a volume placed, its
	// volume made, compose up, running
	stackDir := filepath.Join(t.TempDir(), "shop")
	composeFile := filepath.Join(stackDir, "clowder.yaml")
	if _, err := clowder(t, "--cluster", dir, "stack", "apply", "shop", "-f", composeFile); err == nil {
		t.Error("an apply without the stack's file")
	}
	create := []string{"--cluster", dir, "stack", "create", "shop", "--dir", stackDir}
	if _, err := clowder(t, create...); err == nil || !strings.Contains(err.Error(), "--age is required") {
		t.Errorf("a create without --age: %v", err)
	}
	if _, err := clowder(t, append(create, "--age", identity.Recipient().String())...); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{"clowder.yaml", "secrets.sops.yaml", ".sops.yaml"} {
		if _, err := os.Stat(filepath.Join(stackDir, file)); err != nil {
			t.Errorf("stack create wrote no %s: %v", file, err)
		}
	}
	if _, err := clowder(t, create...); err != nil { // all files there: registered again, nothing written
		t.Errorf("a second create of a stack never applied: %v", err)
	}
	if err := os.WriteFile(composeFile, []byte("name: shop\nservices:\n  app: {image: nginx, volumes: [db:/data]}\n"+
		"volumes:\n  db:\n    x-clowder: {class: fast, size: 1G, replicas: 0}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := clowder(t, "--cluster", dir, "stack", "apply", "shop", "-f", composeFile); err != nil {
		t.Fatal(err)
	}
	lv := compose.VolumeName("shop", "db", 1)
	hostStorage.waitFor(t, "ensure "+lv)
	hostDocker.waitFor(t, "up shop")
	if _, err := os.Stat(filepath.Join(files.Dir, "stacks", "shop", "compose.yaml")); err != nil {
		t.Errorf("the rendered Compose file: %v", err)
	}
	waitForOutput(t, "running", "--cluster", dir, "stack", "ps")
	listed, err := clowder(t, "--cluster", dir, "stack", "ps", "--output", "json")
	var stacks []api.StackInfo
	if err != nil || json.Unmarshal([]byte(listed), &stacks) != nil || len(stacks) != 1 ||
		stacks[0].Name != "shop" || stacks[0].Status != "running" || stacks[0].Host != "node0" {
		t.Errorf("stack ps --output json: %q, %v", listed, err)
	}
	waitForOutput(t, "Up 2 seconds", "--cluster", dir, "stack", "ps", "shop")
	waitForOutput(t, "hello", "--cluster", dir, "stack", "logs", "shop")
	// deleted with its volume: compose down, the LV removed
	if _, err := clowder(t, "--cluster", dir, "stack", "delete", "shop", "--delete-volumes"); err != nil {
		t.Fatal(err)
	}
	hostDocker.waitFor(t, "down shop")
	hostStorage.waitFor(t, "remove "+lv)
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
