package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/webcodingcat/clowder/internal/agent/agenttest"
	"github.com/webcodingcat/clowder/internal/api"
	"github.com/webcodingcat/clowder/internal/buildinfo"
	"github.com/webcodingcat/clowder/internal/cli"
	"github.com/webcodingcat/clowder/internal/clusterdir"
	"github.com/webcodingcat/clowder/internal/hetzner"
	"github.com/webcodingcat/clowder/internal/hetzner/hetznertest"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/network"
	"github.com/webcodingcat/clowder/internal/pki"
	"github.com/webcodingcat/clowder/internal/resource"
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

// waitForTarget fails the test unless the Floating IP comes to the server.
func waitForTarget(t *testing.T, project *hetznertest.API, ip netip.Addr, serverID int64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if project.FloatingIPTarget(ip) == serverID {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("the Floating IP %s is not on server %d", ip, serverID)
}

// fakeNetwork stands in for the host's network and remembers the rules and
// the meshes; network's tests cover what applying them does.
type fakeNetwork struct {
	mu       sync.Mutex
	rulesets []network.Ruleset
	meshes   []network.Mesh
	// interfaceFails and resolverFails make the interface and the resolver
	// fail to start
	interfaceFails, resolverFails bool
	// resolver counts how often the resolver was stopped
	resolver closeCounter
}

func (f *fakeNetwork) SetUpInterface(context.Context, netip.Addr) error {
	if f.interfaceFails {
		return errors.New("no such device")
	}
	return nil
}

func (f *fakeNetwork) ApplyRules(_ context.Context, ruleset network.Ruleset) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rulesets = append(f.rulesets, ruleset)
	return nil
}

func (f *fakeNetwork) ApplyMesh(_ context.Context, mesh network.Mesh) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.meshes = append(f.meshes, mesh)
	return nil
}

func (f *fakeNetwork) AllowForwards(context.Context) error { return nil }

func (f *fakeNetwork) ForgetForwards(context.Context, netip.Addr, []network.Forward) error {
	return nil
}

func (f *fakeNetwork) StartResolver() (io.Closer, error) {
	if f.resolverFails {
		return nil, errors.New("address in use")
	}
	return &f.resolver, nil
}

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
	return clowderWithInput(t, "", args...)
}

// clowderWithInput runs the CLI with input on its stdin.
func clowderWithInput(t *testing.T, input string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	root := cli.NewRoot()
	root.SetOut(&out)
	root.SetIn(strings.NewReader(input))
	root.SetArgs(args)
	err := root.ExecuteContext(context.Background())
	return out.String(), err
}

// closeCounter counts its closes, for what the loops use.
type closeCounter struct{ closed atomic.Int32 }

func (c *closeCounter) Close() error {
	c.closed.Add(1)
	return nil
}

func TestStoppingWaitsForTheLoopsButNotForever(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, stop := context.WithCancel(context.Background())
	var loops sync.WaitGroup
	var used closeCounter
	ended := false
	loops.Go(func() {
		<-ctx.Done()
		time.Sleep(20 * time.Millisecond) // a pass finishing its last step
		if used.closed.Load() > 0 {
			t.Error("closed what the loop uses before it ended")
		}
		ended = true
	})
	if err := stopLoops(stop, &loops, time.Second, logger, &used); err != nil || !ended {
		t.Errorf("stopped before the loop ended: %v", err)
	}
	if used.closed.Load() != 1 {
		t.Errorf("closed what the loops use %d times, want once", used.closed.Load())
	}

	// a loop that hangs (a host tool left behind) does not hold up the
	// stop, and keeps what it uses open
	hung := make(chan struct{})
	defer close(hung)
	var stuck sync.WaitGroup
	var stillUsed closeCounter
	stuck.Go(func() { <-hung })
	if err := stopLoops(func() {}, &stuck, 20*time.Millisecond, logger, &stillUsed); !errors.Is(err, errLoopsLeft) {
		t.Errorf("a hung loop: want errLoopsLeft, got %v", err)
	}
	if stillUsed.closed.Load() != 0 {
		t.Error("closed what a hung loop still uses")
	}
}

// initTestHost runs init for a host on 127.0.0.1, with its public address
// on 127.0.0.2, and returns its files and its Raft and API ports.
func initTestHost(t *testing.T) (files Files, raftPort, apiPort int) {
	t.Helper()
	loopback := netip.MustParseAddr("127.0.0.1")
	files = Files{Dir: t.TempDir(), SecretsDir: t.TempDir()}
	raftPort, apiPort = freePort(t), freePort(t)
	err := Init(context.Background(), files, InitOptions{
		BootstrapKey: testBootstrapKey, Name: "node0", Underlay: loopback, PublicAddress: netip.MustParseAddr("127.0.0.2"),
		ServerID: 1000001, NodeAddress: loopback, RaftPort: raftPort, APIPort: apiPort,
	}, &fakeNetwork{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return files, raftPort, apiPort
}

// listenOn tells whether a port on 127.0.0.1 is free: Raft's port is once
// the agent closed its Raft member.
func listenOn(port int) error {
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return err
	}
	return listener.Close()
}

func TestRunEndsAtItsStartWhenAPartFailsAndLeavesNothingOpen(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	tests := []struct {
		name string
		// prepare breaks one part; it returns what the error must contain
		prepare func(t *testing.T, files *Files, hostNetwork *fakeNetwork, apiPort int) string
		// resolverStarts: the resolver ran before the failure and must be
		// closed after it
		resolverStarts bool
	}{
		{"no directory for secrets", func(_ *testing.T, files *Files, _ *fakeNetwork, _ int) string {
			files.SecretsDir = ""
			return "a directory for the stacks' secrets"
		}, false},
		{"no host file", func(t *testing.T, files *Files, _ *fakeNetwork, _ int) string {
			files.Dir = t.TempDir() // no init ran there
			return "no such file"
		}, false},
		{"the interface fails", func(_ *testing.T, _ *Files, hostNetwork *fakeNetwork, _ int) string {
			hostNetwork.interfaceFails = true
			return "no such device"
		}, false},
		{"the resolver fails", func(_ *testing.T, _ *Files, hostNetwork *fakeNetwork, _ int) string {
			hostNetwork.resolverFails = true
			return "address in use"
		}, false},
		{"the API's port is taken", func(t *testing.T, _ *Files, _ *fakeNetwork, apiPort int) string {
			taken, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(apiPort)))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = taken.Close() })
			return "address already in use"
		}, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel() // init takes a while
			files, raftPort, apiPort := initTestHost(t)
			hostNetwork, hostDocker := &fakeNetwork{}, &agenttest.Docker{}
			want := test.prepare(t, &files, hostNetwork, apiPort)
			parts := HostParts{
				Network: hostNetwork, Storage: &agenttest.Storage{Dir: t.TempDir()}, Docker: hostDocker,
			}
			err := Run(context.Background(), files, parts, logger)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("Run: %v, want an error with %q", err, want)
			}
			if err := listenOn(raftPort); err != nil {
				t.Errorf("Raft's port is still taken: %v", err)
			}
			wantClosed := int32(0)
			if test.resolverStarts {
				wantClosed = 1
			}
			if closed := hostNetwork.resolver.closed.Load(); closed != wantClosed {
				t.Errorf("the resolver was closed %d times, want %d", closed, wantClosed)
			}
			if calls := hostDocker.Lines(); len(calls) != 0 {
				t.Errorf("Docker was asked %q", calls)
			}
		})
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
		BootstrapKey: testBootstrapKey, Name: "node0", Underlay: loopback,
		PublicAddress: netip.MustParseAddr("127.0.0.2"), ServerID: 1000001,
		NodeAddress: loopback, RaftPort: freePort(t), APIPort: apiPort,
	}, hostNetwork, logger)
	if err != nil {
		t.Fatal(err)
	}
	if err := Init(context.Background(), files, InitOptions{BootstrapKey: testBootstrapKey, Name: "node0", Underlay: loopback},
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
	hostStorage := &agenttest.Storage{Dir: t.TempDir()}
	hostDocker := &agenttest.Docker{}
	project := hetznertest.New(t, "CHANGE-ME")
	project.AddServer(1000001, loopback, netip.MustParseAddr("127.0.0.2"))
	floatingIP := netip.MustParseAddr("203.0.113.10")
	project.RouteFloatingIP(floatingIP, 0)
	parts := HostParts{Network: hostNetwork, Storage: hostStorage, Docker: hostDocker}
	go func() { stopped <- Run(ctx, files, parts, logger) }()
	stopAgent := sync.OnceValue(func() error {
		stop()
		return <-stopped
	})
	defer func() {
		if err := stopAgent(); err != nil {
			t.Errorf("the agent stopped with %v", err)
		}
	}()
	message := make([]byte, 64)
	_ = systemd.SetReadDeadline(time.Now().Add(30 * time.Second))
	if n, err := systemd.Read(message); err != nil || string(message[:n]) != "READY=1" {
		t.Fatalf("systemd heard %q, %v", message[:n], err)
	}

	// the user's side: an empty cluster directory, a config directory
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv(clusterdir.EnvVar, "")
	dir := t.TempDir()
	endpoint := net.JoinHostPort("127.0.0.2", strconv.Itoa(apiPort))
	recoveryFile := filepath.Join(t.TempDir(), "recovery-key")
	bootstrap := func(user, name, cluster, key string, more ...string) error {
		_, err := clowder(t, append([]string{
			"--cluster", cluster, "cluster", "bootstrap", user, "--name", name,
			"--endpoint", endpoint, "--key-file", key,
		}, more...)...)
		return err
	}

	if err := bootstrap("alice", "test", dir, keyFile(t, "a-guess-that-is-long-enough-to-be-tried"), "--recovery-out", recoveryFile); err == nil ||
		!strings.Contains(err.Error(), "cannot prove it knows the bootstrap key") {
		t.Errorf("a wrong bootstrap key: %v", err)
	}
	theKey := keyFile(t, testBootstrapKey+"\n")
	if err := bootstrap("alice", "test", dir, theKey); err == nil || !strings.Contains(err.Error(), "--recovery-out") {
		t.Errorf("no terminal and no --recovery-out: %v", err)
	}
	if err := bootstrap("alice", "test", dir, theKey, "--recovery-out", recoveryFile); err != nil {
		t.Fatal(err)
	}
	if recovery, err := os.ReadFile(recoveryFile); err != nil || !strings.HasPrefix(string(recovery), "AGE-SECRET-KEY-1") {
		t.Errorf("the recovery key: %q, %v", recovery, err)
	}
	if err := bootstrap("bob", "test", t.TempDir(), theKey, "--recovery-out", filepath.Join(t.TempDir(), "r")); err == nil ||
		!strings.Contains(err.Error(), "--force") {
		t.Errorf("a second bootstrap under the same name: %v", err)
	}
	secondRecovery := filepath.Join(t.TempDir(), "r")
	if err := bootstrap("bob", "other", t.TempDir(), theKey, "--recovery-out", secondRecovery); err == nil ||
		!strings.Contains(err.Error(), "has a user already") {
		t.Errorf("a second bootstrap: %v", err)
	}
	if _, err := os.Stat(secondRecovery); err == nil {
		t.Error("a second bootstrap made a recovery key")
	}

	// cluster.yaml as the bootstrap wrote it, without the bootstrap key,
	// which opens nothing any more: not even a proof of the CA
	config := filepath.Join(dir, clusterdir.ConfigFile)
	written, err := os.ReadFile(config)
	if err != nil || !strings.Contains(string(written), "name: test\n") ||
		strings.Contains(string(written), testBootstrapKey) || strings.Contains(string(written), "BEGIN AGE") {
		t.Fatalf("cluster.yaml: %q, %v", written, err)
	}
	offer, err := api.FindCluster(context.Background(), endpoint, api.DefaultConnectTimeout, []byte(testBootstrapKey), buildinfo.Version)
	if !errors.Is(err, api.ErrBootstrapped) || offer != nil {
		t.Errorf("the bootstrap key after the bootstrap: %v, %v", offer, err)
	}
	if out, err := clowder(t, "--cluster", dir, "cluster", "diff"); err == nil || !strings.Contains(err.Error(), "hetzner") {
		t.Errorf("diff of the unfinished cluster.yaml: %q, %v", out, err)
	}
	// finished with the Hetzner token, encrypted by the cluster's
	// recipients; the hosts' private network is the loopback's here
	finish := func(token string, privateNetwork string) {
		t.Helper()
		block, err := clowderWithInput(t, token+"\n", "--cluster", dir, "secret", "encrypt", "--cluster-yaml")
		if err != nil {
			t.Fatal(err)
		}
		finished := string(written) + "hetzner:\n  api: " + project.URL + "\n  token: " + block +
			"private_network: " + privateNetwork + "\n" +
			"ingress:\n  floating_ip: 203.0.113.10\nstorage_classes:\n  fast: {pool: vg-fast/thin}\n"
		if err := os.WriteFile(config, []byte(finished), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	finish("CHANGE-ME", "127.0.0.0/8")
	if out, err := clowder(t, "--cluster", dir, "cluster", "diff"); err != nil || !strings.Contains(out, "never applied") ||
		!strings.Contains(out, "the secret hetzner.token differs") {
		t.Errorf("diff before the first apply: %q, %v", out, err)
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
	waitForTarget(t, project, floatingIP, 1000001)
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
	hostStorage.WaitFor(t, "ensure "+lv)
	hostDocker.WaitFor(t, "up default_shop")
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
	hostDocker.WaitFor(t, "down default_shop")
	hostStorage.WaitFor(t, "remove "+lv)
	// the same token encrypted anew is no change; the private network is
	finish("CHANGE-ME", "127.0.0.0/16")
	if out, err := clowder(t, "--cluster", dir, "cluster", "diff"); err != nil || !strings.Contains(out, "private_network") ||
		strings.Contains(out, "secret") {
		t.Errorf("diff after a change of cluster.yaml: %q, %v", out, err)
	}
	// and now a new Hetzner token as well: diff names both
	finish("new", "127.0.0.0/16")
	if out, err := clowder(t, "--cluster", dir, "cluster", "diff"); err != nil || !strings.Contains(out, "private_network") ||
		!strings.Contains(out, "the secret hetzner.token differs") {
		t.Errorf("diff after both changed: %q, %v", out, err)
	}
	out, err = clowder(t, "--cluster", dir, "cluster", "status")
	if err != nil || !strings.Contains(out, "leader node0") || !strings.Contains(out, "revision 1") {
		t.Errorf("status: %q, %v", out, err)
	}

	// a server clowderd recover retired: its agent starts nothing, not even
	// the node's interface, which would fail here
	if err := stopAgent(); err != nil {
		t.Fatal(err)
	}
	credentials, err := files.readCredentials()
	if err != nil {
		t.Fatal(err)
	}
	setRetiredLabel := func(serial string) { project.SetLabel(1000001, hetzner.RetiredLabel, serial) }
	setRetiredLabel(pki.Serial(credentials.certificate.Leaf))
	parts.Network = &fakeNetwork{interfaceFails: true}
	if err := Run(context.Background(), files, parts, logger); !errors.Is(err, ErrRetired) {
		t.Errorf("the agent of a retired server: %v", err)
	}
	// the label names another host's certificate: the server was set up
	// anew, and its agent starts (up to the interface that fails here)
	setRetiredLabel("another")
	if err := Run(context.Background(), files, parts, logger); err == nil || errors.Is(err, ErrRetired) {
		t.Errorf("the agent of a server set up anew: %v", err)
	}
}
