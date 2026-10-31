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

	"github.com/webcodingcat/clowder/internal/cli"
	"github.com/webcodingcat/clowder/internal/clusterdir"
	"github.com/webcodingcat/clowder/internal/clusterspec"
	"github.com/webcodingcat/clowder/internal/docker"
	"github.com/webcodingcat/clowder/internal/hoster"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/network"
	"github.com/webcodingcat/clowder/internal/resource"
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
// fakeHoster is the hoster's API: it remembers where the Floating IP goes
// and with which token it was asked.
type fakeHoster struct {
	mu     sync.Mutex
	token  string
	target string
}

func (f *fakeHoster) connect(_ clusterspec.Hoster, token string) (hoster.Hoster, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.token = token
	return f, nil
}

func (f *fakeHoster) FloatingIPTarget(context.Context, netip.Addr) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.target, nil
}

func (f *fakeHoster) AssignFloatingIP(_ context.Context, _ netip.Addr, serverRef string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.target = serverRef
	return nil
}

func (*fakeHoster) PowerOff(context.Context, string) error { return nil }
func (*fakeHoster) PowerOn(context.Context, string) error  { return nil }

// waitForTarget fails the test unless the Floating IP comes to the server.
func (f *fakeHoster) waitForTarget(t *testing.T, serverRef, token string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		target, usedToken := f.target, f.token
		f.mu.Unlock()
		if target == serverRef && usedToken == token {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("the Floating IP is not on server %s with token %q", serverRef, token)
}

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

// fakeStorage stands in for the storage driver: every pool has 100 GiB,
// dataPercent full, unless reading it fails (poolFails).
type fakeStorage struct {
	calls
	dir         string
	dataPercent float64
	poolFails   bool
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
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.poolFails {
		return 0, 0, 0, errors.New("lvs timed out")
	}
	return 100 << 30, f.dataPercent, 0, nil
}

func (f *fakeStorage) MountPoint(volume storage.Volume) string {
	return filepath.Join(f.dir, volume.LV)
}

// fakeDocker stands in for Docker: it sends no events and has one
// container per stack.
type fakeDocker struct {
	calls
	// failedUps and failedDowns are how many compose ups and downs fail
	// before one works
	failedUps, failedDowns int
	// down makes Ping fail as if refused, hangs as if it timed out
	down, hangs bool
	// upFiles and leftOut are the files and the services left out of the
	// last compose up
	upFiles, leftOut []string
	// containers and details are what Docker lists and inspects
	containers []docker.Container
	details    map[string]docker.Details
}

func (f *fakeDocker) Events(ctx context.Context, _ string) (<-chan docker.Event, <-chan error) {
	events := make(chan docker.Event)
	go func() {
		<-ctx.Done()
		close(events)
	}()
	return events, make(chan error)
}

func (f *fakeDocker) Ping(ctx context.Context) error {
	f.mu.Lock()
	down, hangs := f.down, f.hangs
	f.mu.Unlock()
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case hangs:
		// the client's own timeout ran out
		return fmt.Errorf("ping Docker: %w", context.DeadlineExceeded)
	case down:
		return errors.New("connection refused")
	}
	return nil
}

func (f *fakeDocker) Start(context.Context, string) error   { return nil }
func (f *fakeDocker) Restart(context.Context, string) error { return nil }

func (f *fakeDocker) ComposeUp(_ context.Context, stack string, files, leftOut []string) error {
	f.add("up %s", stack)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.upFiles, f.leftOut = files, leftOut
	if f.failedUps > 0 {
		f.failedUps--
		return errors.New("pull access denied")
	}
	return nil
}

func (f *fakeDocker) ComposePull(_ context.Context, stack string, _, services []string) error {
	f.add("pull %s %s", stack, strings.Join(services, " "))
	return nil
}

func (f *fakeDocker) ComposeDown(_ context.Context, stack, _ string) error {
	f.add("down %s", stack)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failedDowns > 0 {
		f.failedDowns--
		return errors.New("the container does not stop")
	}
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

func (f *fakeDocker) Containers(context.Context, string) ([]docker.Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.containers), nil
}

func (f *fakeDocker) Inspect(_ context.Context, id string) (docker.Details, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	details, ok := f.details[id]
	if !ok {
		return docker.Details{}, docker.ErrNoContainer
	}
	return details, nil
}

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

func TestStoppingWaitsForTheLoopsButNotForever(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, stop := context.WithCancel(context.Background())
	var loops sync.WaitGroup
	ended := false
	loops.Go(func() {
		<-ctx.Done()
		time.Sleep(20 * time.Millisecond) // a pass finishing its last step
		ended = true
	})
	if !stopLoops(stop, &loops, time.Second, logger) || !ended {
		t.Error("stopped before the loop ended")
	}

	// a loop that hangs (a host tool left behind) does not hold up the stop
	hung := make(chan struct{})
	defer close(hung)
	var stuck sync.WaitGroup
	stuck.Go(func() { <-hung })
	if stopLoops(func() {}, &stuck, 20*time.Millisecond, logger) {
		t.Error("a hung loop counted as ended")
	}
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
	hostHoster := &fakeHoster{}
	parts := HostParts{Network: hostNetwork, Storage: hostStorage, Docker: hostDocker, Hoster: hostHoster.connect}
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
	// and the leader routes it to this host's server, with the token opened
	hostHoster.waitForTarget(t, "1000001", "CHANGE-ME")
	if out, err := clowder(t, "--cluster", dir, "cluster", "diff"); err != nil || !strings.Contains(out, "nothing to apply") {
		t.Errorf("diff after apply: %q, %v", out, err)
	}

	// a stack with a volume: applied, the volume placed with it and made,
	// compose up, running
	resources := filepath.Join(t.TempDir(), "shop.yaml")
	if err := os.WriteFile(resources, []byte(`apiVersion: clowder/v1
kind: Volume
metadata: {name: db}
spec: {class: fast, size: 1G, replicas: 0}
---
apiVersion: clowder/v1
kind: Stack
metadata: {name: shop}
spec: {services: {app: {image: nginx, volumes: [{volume: db, path: /data}]}}}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := clowder(t, "--cluster", dir, "apply", "-f", resources); err != nil {
		t.Fatal(err)
	}
	lv := names.VolumeName("default/db", 1)
	hostStorage.waitFor(t, "ensure "+lv)
	hostDocker.waitFor(t, "up default_shop")
	if _, err := os.Stat(filepath.Join(files.Dir, "stacks", "default_shop", "compose.yaml")); err != nil {
		t.Errorf("the rendered Compose file: %v", err)
	}
	waitForOutput(t, "running", "--cluster", dir, "get", "stacks")
	listed, err := clowder(t, "--cluster", dir, "get", "stacks", "-o", "json")
	var stacks []resource.Object
	var status struct{ Status, Host string }
	if err != nil || json.Unmarshal([]byte(listed), &stacks) != nil || len(stacks) != 1 ||
		stacks[0].Metadata.Name != "shop" || json.Unmarshal(stacks[0].Status, &status) != nil ||
		status.Status != "running" || status.Host != "node0" {
		t.Errorf("get stacks -o json: %q, %v", listed, err)
	}
	waitForOutput(t, "Up 2 seconds", "--cluster", dir, "stack", "ps", "shop")
	waitForOutput(t, "hello", "--cluster", dir, "stack", "logs", "shop")
	// deleted, the stack before its volume: compose down, the LV removed
	if _, err := clowder(t, "--cluster", dir, "delete", "--with-data", "-f", resources); err != nil {
		t.Fatal(err)
	}
	hostDocker.waitFor(t, "down default_shop")
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
