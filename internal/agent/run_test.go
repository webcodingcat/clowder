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
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/webcodingcat/clowder/internal/agent/agenttest"
	"github.com/webcodingcat/clowder/internal/cli"
	"github.com/webcodingcat/clowder/internal/clientconfig"
	"github.com/webcodingcat/clowder/internal/hetzner"
	"github.com/webcodingcat/clowder/internal/hetzner/hetznertest"
	"github.com/webcodingcat/clowder/internal/hostrole"
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
		ClusterName: testClusterName, Name: "node0", Roles: hostrole.DefaultInit, Underlay: loopback,
		PublicAddress: netip.MustParseAddr("127.0.0.2"), ServerID: 1000001, NodeAddress: loopback, RaftPort: raftPort, APIPort: apiPort,
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

// TestAFirstHostAndItsCLI goes the way of a new cluster: init on the host,
// the agent running, the first credentials from the host and new ones,
// apply and diff.
func TestAFirstHostAndItsCLI(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	loopback := netip.MustParseAddr("127.0.0.1")
	files := Files{Dir: t.TempDir(), SecretsDir: t.TempDir()}
	apiPort := freePort(t)
	hostNetwork := &fakeNetwork{}
	// the public address is another loopback address, as a host's public
	// address is another than its underlay; users reach the API there
	err := Init(context.Background(), files, InitOptions{
		ClusterName: testClusterName, Name: "node0", Roles: hostrole.DefaultInit, Underlay: loopback,
		PublicAddress: netip.MustParseAddr("127.0.0.2"), ServerID: 1000001,
		NodeAddress: loopback, RaftPort: freePort(t), APIPort: apiPort,
	}, hostNetwork, logger)
	if err != nil {
		t.Fatal(err)
	}
	if err := Init(context.Background(), files,
		InitOptions{ClusterName: testClusterName, Name: "node0", Roles: hostrole.DefaultInit, Underlay: loopback},
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

	// the CLI's side: a credentials file of its own, nothing in it yet
	t.Setenv(clientconfig.EnvVar, filepath.Join(t.TempDir(), "config"))
	dir := t.TempDir()
	if _, err := clowder(t, "host", "list"); err == nil || !strings.Contains(err.Error(), "clowderd credentials") {
		t.Errorf("the CLI without credentials: %v", err)
	}
	// root on the host prints the first credentials, as over SSH: the
	// public addresses of the hosts as the endpoints, unless --endpoint
	// names others (here the API's port, which a test cannot have fixed)
	var printed bytes.Buffer
	if err := printCredentials(context.Background(), &printed, files, CredentialsOptions{Name: "alice"}); err != nil {
		t.Fatal(err)
	}
	first, err := clientconfig.Parse(printed.Bytes())
	if err != nil || first.CurrentContext != testClusterName ||
		!slices.Equal(first.Contexts[0].Endpoints, []string{"127.0.0.2"}) {
		t.Fatalf("the printed credentials: %+v, %v", first, err)
	}
	endpoint := net.JoinHostPort("127.0.0.2", strconv.Itoa(apiPort))
	printed.Reset()
	err = printCredentials(context.Background(), &printed, files, CredentialsOptions{Name: "alice", Endpoints: []string{endpoint}})
	if err != nil {
		t.Fatal(err)
	}
	if out, err := clowderWithInput(t, printed.String(), "config", "import", "-"); err != nil {
		t.Fatalf("import: %q, %v", out, err)
	}
	if _, err := clowder(t, "host", "list"); err != nil {
		t.Errorf("the first credentials: %v", err)
	}
	// root on the host exports the sealing key, whose public half the
	// cluster names for secret seal
	var sealingKey, publicKey bytes.Buffer
	if err := printSealingKey(context.Background(), &sealingKey, files, false); err != nil ||
		!strings.HasPrefix(sealingKey.String(), "AGE-SECRET-KEY-1") {
		t.Errorf("the sealing key: %q, %v", sealingKey.String(), err)
	}
	if err := printSealingKey(context.Background(), &publicKey, files, true); err != nil {
		t.Fatal(err)
	}
	identity, err := age.ParseX25519Identity(strings.TrimSpace(sealingKey.String()))
	if err != nil || strings.TrimSpace(publicKey.String()) != identity.Recipient().String() {
		t.Errorf("the public sealing key %q does not belong to the sealing key: %v", publicKey.String(), err)
	}

	// root on the host hands out new credentials and, with --revoke-older,
	// locks out the first. A certificate of the same second as the new one
	// stays valid (certificates count in seconds): wait for the next.
	time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(time.Second)))
	printed.Reset()
	err = printCredentials(context.Background(), &printed, files,
		CredentialsOptions{Name: "bob", Endpoints: []string{endpoint}, RevokeOlder: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := clowder(t, "host", "list"); err == nil || !strings.Contains(err.Error(), "revoke-older") {
		t.Errorf("the first credentials after --revoke-older: %v", err)
	}
	if _, err := clowderWithInput(t, printed.String(), "config", "import", "-", "--as", "bob"); err != nil {
		t.Fatal(err)
	}
	if _, err := clowder(t, "config", "use-context", "bob"); err != nil {
		t.Fatal(err)
	}
	if out, err := clowder(t, "cluster", "status"); err != nil || !strings.Contains(out, "cluster "+testClusterName) {
		t.Errorf("the host's new credentials: %q, %v", out, err)
	}

	// the configuration from scratch, after the Secret with the Hetzner
	// token, sealed for the cluster; the hosts' private network is the
	// loopback's
	config := filepath.Join(dir, "configuration.yaml")
	finish := func(token string, privateNetwork string) {
		t.Helper()
		secret, err := clowder(t, "secret", "seal", "clowder-system/hetzner", "--from-literal", "token="+token)
		if err != nil {
			t.Fatal(err)
		}
		finished := secret + "---\napiVersion: clowder/v1\nkind: ClusterConfiguration\nmetadata: {name: cluster}\nspec:\n" +
			"  hetzner: {api: " + project.URL + ", tokenSecretRef: {name: hetzner, key: token}}\n" +
			"  privateNetwork: " + privateNetwork + "\n" +
			"  ingress: {floatingIP: 203.0.113.10}\n  storageClasses: {fast: {pool: vg-fast/thin}}\n"
		if err := os.WriteFile(config, []byte(finished), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	finish("CHANGE-ME", "127.0.0.0/8")
	// differences end diff with exit code 1 and no message (cli.Silent)
	if out, err := clowder(t, "diff", "-f", config); !cli.Silent(err) ||
		!strings.Contains(out, "ClusterConfiguration cluster: new") || !strings.Contains(out, "SealedSecret clowder-system/hetzner: new") {
		t.Errorf("diff before the first apply: %q, %v", out, err)
	}
	// a new Hetzner token: without a terminal, only with --yes
	if _, err := clowder(t, "apply", "-f", config); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Errorf("apply of a new token without --yes: %v", err)
	}
	out, err := clowder(t, "apply", "--yes", "-f", config)
	if err != nil || !strings.Contains(out, "ClusterConfiguration cluster: applied") {
		t.Fatalf("apply: %q, %v", out, err)
	}
	// the Floating IP of the configuration reaches the nft rules (HTTP hairpin)
	hostNetwork.waitForRuleset(t, func(ruleset network.Ruleset) bool {
		return ruleset.FloatingIP == netip.MustParseAddr("203.0.113.10")
	})
	// and the leader routes it to this host's server, with the token opened
	waitForTarget(t, project, floatingIP, 1000001)
	if out, err := clowder(t, "diff", "-f", config); err != nil || !strings.Contains(out, "nothing would change") {
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
	if _, err := clowder(t, "apply", "-f", resources); err != nil {
		t.Fatal(err)
	}
	lv := names.VolumeName("default/db", 1)
	hostStorage.WaitFor(t, "ensure "+lv)
	hostDocker.WaitFor(t, "up default_shop")
	if _, err := os.Stat(filepath.Join(files.Dir, "stacks", "default_shop", "compose.yaml")); err != nil {
		t.Errorf("the rendered Compose file: %v", err)
	}
	waitForOutput(t, "running", "get", "stacks")
	listed, err := clowder(t, "get", "stacks", "-o", "json")
	var stacks []resource.Object
	var status struct{ Status, Host string }
	if err != nil || json.Unmarshal([]byte(listed), &stacks) != nil || len(stacks) != 1 ||
		stacks[0].Metadata.Name != "shop" || json.Unmarshal(stacks[0].Status, &status) != nil ||
		status.Status != "running" || status.Host != "node0" {
		t.Errorf("get stacks -o json: %q, %v", listed, err)
	}
	waitForOutput(t, "Up 2 seconds", "stack", "ps", "shop")
	waitForOutput(t, "hello", "stack", "logs", "shop")
	// deleted, the stack before its volume: compose down, the LV removed
	if _, err := clowder(t, "delete", "--with-data", "-f", resources); err != nil {
		t.Fatal(err)
	}
	hostDocker.WaitFor(t, "down default_shop")
	hostStorage.WaitFor(t, "remove "+lv)
	// the same token encrypted anew is no change; the private network is
	finish("CHANGE-ME", "127.0.0.0/16")
	if out, err := clowder(t, "diff", "-f", config); !cli.Silent(err) || !strings.Contains(out, "spec.privateNetwork") ||
		strings.Contains(out, "Secret") {
		t.Errorf("diff after a change of the configuration: %q, %v", out, err)
	}
	// and now a new Hetzner token as well: diff names both
	finish("new", "127.0.0.0/16")
	if out, err := clowder(t, "diff", "-f", config); !cli.Silent(err) || !strings.Contains(out, "spec.privateNetwork") ||
		!strings.Contains(out, "Secret clowder-system/hetzner: differs in data.token") {
		t.Errorf("diff after both changed: %q, %v", out, err)
	}
	out, err = clowder(t, "cluster", "status")
	if err != nil || !strings.Contains(out, "leader node0") || !strings.Contains(out, "configuration generation 1") {
		t.Errorf("status: %q, %v", out, err)
	}

	// a server clowderd recover retired: its agent starts nothing, not even
	// the node's interface, which would fail here
	if err := stopAgent(); err != nil {
		t.Fatal(err)
	}
	credentials, err := files.readCredentials(hostrole.DefaultInit)
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
