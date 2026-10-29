package agent

import (
	"context"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/webcodingcat/clowder/internal/clusterdir"
	"github.com/webcodingcat/clowder/internal/compose"
	"github.com/webcodingcat/clowder/internal/state"
	"github.com/webcodingcat/clowder/internal/supervisor"
)

// testCluster is a state that applies what is proposed, as the leader
// would; the runner's host is host 0.
type testCluster struct {
	t     *testing.T
	mu    sync.Mutex
	state *state.State
}

func (c *testCluster) Read(use func(current *state.State)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	use(c.state)
}

func (c *testCluster) Changed() <-chan struct{} { return make(chan struct{}) }

func (c *testCluster) propose(_ context.Context, cmd state.Command) (uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cmd.At = time.Now()
	return c.state.Index + 1, c.state.Apply(c.state.Index+1, cmd)
}

// apply commits a command and fails the test if the state refuses it.
func (c *testCluster) apply(kind, actor string, payload any) {
	c.t.Helper()
	cmd, err := state.NewCommand(kind, time.Now(), actor, payload)
	if err != nil {
		c.t.Fatal(err)
	}
	if _, err := c.propose(context.Background(), cmd); err != nil {
		c.t.Fatal(err)
	}
}

func (c *testCluster) stack(name string) state.Stack {
	c.mu.Lock()
	defer c.mu.Unlock()
	return *c.state.Stacks[name]
}

// newTestRunner is a runner on host 0 of a cluster with the stack "shop":
// one service and the volume "db" (generation 1), placed on host 0.
func newTestRunner(t *testing.T) (*stackRunner, *testCluster, *fakeDocker, *fakeStorage) {
	t.Helper()
	cluster := &testCluster{t: t, state: &state.State{}}
	cluster.apply(state.KindBootstrap, state.LeaderActor, state.Bootstrap{
		Keys:    state.ClusterKeys{CACertificate: []byte("ca"), CAKey: state.Sealed("k"), AgeIdentity: state.Sealed("a")},
		JoinKey: state.Sealed("j"),
		Host: state.Host{
			ID: 0, Name: "node0", Underlay: netip.MustParseAddr("10.0.0.2"), WGPublicKey: "wg", CertificateSerial: "s0",
		},
	})
	cluster.apply(state.KindApplyConfig, state.UserActor("alice"), state.ApplyConfig{
		Spec: clusterdir.Config{
			Name: "prod", Seeds: []string{"10.0.0.2"},
			Hoster:         clusterdir.Hoster{Type: "hetzner", Token: clusterdir.SecretRef{Secret: "token"}},
			Ingress:        clusterdir.Ingress{FloatingIP: "203.0.113.10"},
			StorageClasses: map[string]clusterdir.StorageClass{"fast": {Pool: "vg-fast/thin"}},
		},
		Secrets: map[string]state.Sealed{"token": state.Sealed("t")},
	})
	cluster.apply(state.KindCreateStack, state.UserActor("alice"), state.CreateStack{Name: "shop", Owner: state.OwnerCLI})
	cluster.apply(state.KindPutStack, state.UserActor("alice"), state.PutStack{
		Name: "shop", Owner: state.OwnerCLI, ConfigRevision: 1,
		Source: "name: shop\nservices: {app: {image: nginx, volumes: [db:/data]}}\n" +
			"volumes:\n  db:\n    x-clowder: {class: fast, size: 1G, replicas: 0}\n",
	})
	putVolume(cluster, 1)
	host := state.HostID(0)
	cluster.apply(state.KindSetStackStatus, state.LeaderActor, state.SetStackStatus{
		Name: "shop", Status: state.StackStarting, Host: &host,
	})

	docker, storage := &fakeDocker{}, &fakeStorage{dir: t.TempDir()}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	runner := &stackRunner{
		files: Files{Dir: t.TempDir(), SecretsDir: t.TempDir()}, host: Host{ID: 0, Name: "node0", NodeAddress: netip.MustParseAddr("10.77.0.1")},
		version: "1.0.0", cluster: cluster, docker: docker, storage: storage, logger: logger,
		propose: cluster.propose, applied: map[string][32]byte{}, logins: map[string][32]byte{},
		// the tests' sealed values are their clear text
		open:      func(sealed state.Sealed) ([]byte, error) { return []byte(sealed), nil },
		digestKey: []byte("the test's digest key"),
	}
	runner.supervisor = supervisor.New(docker, runner.rules, supervisor.DefaultBackoff, nil, logger)
	return runner, cluster, docker, storage
}

func putVolume(cluster *testCluster, generation int64) {
	cluster.apply(state.KindPutVolume, state.LeaderActor, state.PutVolume{Volume: state.Volume{
		ID: "shop/db", Generation: generation, Class: "fast", SizeBytes: 1 << 30,
		Placements: []state.Placement{{Host: 0, Class: "fast"}}, Status: state.VolumeOK,
	}})
}

func (c *calls) count(line string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(slices.DeleteFunc(slices.Clone(c.lines), func(l string) bool { return l != line }))
}

func TestAFailedComposeUpIsTriedAgain(t *testing.T) {
	runner, cluster, docker, _ := newTestRunner(t)
	docker.failedUps = 1
	runner.pass(context.Background())
	if status := cluster.stack("shop").Status; status != state.StackStarting {
		t.Errorf("after a failed compose up the stack is %s", status)
	}
	runner.pass(context.Background())
	if ups := docker.count("up shop"); ups != 2 {
		t.Errorf("%d compose ups, want 2", ups)
	}
	if status := cluster.stack("shop").Status; status != state.StackRunning {
		t.Errorf("after compose up the stack is %s", status)
	}
	runner.pass(context.Background()) // nothing changed: no compose up
	if ups := docker.count("up shop"); ups != 2 {
		t.Errorf("%d compose ups without a change, want 2", ups)
	}
	runner.forgetApplied() // Docker restarted
	runner.pass(context.Background())
	if ups := docker.count("up shop"); ups != 3 {
		t.Errorf("%d compose ups after Docker restarted, want 3", ups)
	}
}

func TestANewGenerationReplacesTheLV(t *testing.T) {
	runner, cluster, docker, storage := newTestRunner(t)
	runner.pass(context.Background())
	putVolume(cluster, 2) // a restore
	runner.pass(context.Background())
	old, current := compose.VolumeName("shop", "db", 1), compose.VolumeName("shop", "db", 2)
	if storage.count("ensure "+current) != 1 || storage.count("remove "+old) != 1 || storage.count("remove "+current) != 0 {
		t.Errorf("storage calls %q", storage.lines)
	}
	if ups := docker.count("up shop"); ups != 2 {
		t.Errorf("%d compose ups, want 2 (the new volume)", ups)
	}
}

func TestAStackThatLeftIsStoppedAndForgotten(t *testing.T) {
	runner, cluster, docker, storage := newTestRunner(t)
	runner.pass(context.Background())

	// moved off this host, but its volume copy stays here for now
	cluster.apply(state.KindSetStackStatus, state.LeaderActor, state.SetStackStatus{Name: "shop", Status: state.StackPending})
	runner.pass(context.Background())
	runner.pass(context.Background())
	lv := compose.VolumeName("shop", "db", 1)
	if docker.count("down shop") != 1 || storage.count("remove "+lv) != 0 {
		t.Errorf("docker %q, storage %q", docker.lines, storage.lines)
	}

	cluster.apply(state.KindDeleteStack, state.UserActor("alice"), state.DeleteStack{Name: "shop", WithVolumes: true})
	runner.pass(context.Background())
	if storage.count("remove "+lv) != 1 {
		t.Errorf("storage %q", storage.lines)
	}
	if _, err := os.Stat(runner.files.stackDir("shop")); !os.IsNotExist(err) {
		t.Errorf("the stack's directory is left: %v", err)
	}
}

// putShopWithSecrets applies shop again: app asks for db-password as a file
// and, with apiKey set, for api-key as the variable API_KEY.
func putShopWithSecrets(cluster *testCluster, password, apiKey string) {
	app := "{image: nginx, volumes: [db:/data], x-clowder: {secrets: [db-password]}}"
	secrets := map[string]state.Sealed{"db-password": state.Sealed(password)}
	if apiKey != "" {
		app = "{image: nginx, volumes: [db:/data], x-clowder: {secrets: [db-password], secret_env: {API_KEY: api-key}}}"
		secrets["api-key"] = state.Sealed(apiKey)
	}
	cluster.apply(state.KindPutStack, state.UserActor("alice"), state.PutStack{
		Name: "shop", Owner: state.OwnerCLI, Secrets: secrets, ConfigRevision: 1,
		Source: "name: shop\nservices:\n  app: " + app + "\nvolumes:\n  db:\n    x-clowder: {class: fast, size: 1G, replicas: 0}\n",
	})
}

func TestSecretsReachTheHostAndLeaveWithTheStack(t *testing.T) {
	runner, cluster, docker, _ := newTestRunner(t)
	password := runner.files.secretFile("shop", "db-password")
	env := runner.files.secretEnvFile("shop")

	putShopWithSecrets(cluster, "first", "key")
	runner.pass(context.Background())
	if content, err := os.ReadFile(password); err != nil || string(content) != "first" {
		t.Errorf("the secret file: %q, %v", content, err)
	}
	firstLabel := secretsLabel(t, runner)
	if info, err := os.Stat(password); err != nil || info.Mode().Perm() != 0o444 {
		t.Errorf("the secret file's mode: %v, %v", info, err)
	}
	if content, err := os.ReadFile(env); err != nil || !strings.Contains(string(content), "API_KEY: key") {
		t.Errorf("the secret variables: %q, %v", content, err)
	}
	if len(docker.upFiles) != 2 || docker.upFiles[1] != env {
		t.Errorf("compose up with %v", docker.upFiles)
	}

	// a new password and no variable any more: rewritten, env.yaml gone
	putShopWithSecrets(cluster, "second", "")
	runner.pass(context.Background())
	if content, err := os.ReadFile(password); err != nil || string(content) != "second" {
		t.Errorf("the changed secret file: %q, %v", content, err)
	}
	// the file keeps its path: only the label makes compose recreate app
	if secretsLabel(t, runner) == firstLabel {
		t.Error("the label of app did not change with its secret file")
	}
	if _, err := os.Stat(env); !os.IsNotExist(err) {
		t.Errorf("the secret variables stayed: %v", err)
	}
	if ups := docker.count("up shop"); ups != 2 || len(docker.upFiles) != 1 {
		t.Errorf("%d compose ups, the last with %v", ups, docker.upFiles)
	}

	cluster.apply(state.KindSetStackStatus, state.LeaderActor, state.SetStackStatus{Name: "shop", Status: state.StackPending})
	runner.pass(context.Background())
	if _, err := os.Stat(runner.files.secretsDir("shop")); !os.IsNotExist(err) {
		t.Errorf("the secrets of a stack that left stayed: %v", err)
	}
}

// secretsLabel reads the digest label of app from shop's rendered file.
func secretsLabel(t *testing.T, runner *stackRunner) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(runner.files.stackDir("shop"), "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var rendered struct {
		Services map[string]struct {
			Labels map[string]string `yaml:"labels"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(content, &rendered); err != nil {
		t.Fatal(err)
	}
	label := rendered.Services["app"].Labels[compose.LabelSecrets]
	if label == "" {
		t.Fatal("app has no digest of its secret files")
	}
	return label
}

func TestSecretDigestsAreKeyedAndUnambiguous(t *testing.T) {
	values := map[string]string{"a": "bc", "ab": "c", "x": "1"}
	digest := secretDigest([]byte("key"), values, []string{"a", "x"})
	if digest != secretDigest([]byte("key"), values, []string{"x", "a"}) {
		t.Error("the order of the names changed the digest")
	}
	for what, other := range map[string]string{
		"another key":                    secretDigest([]byte("other"), values, []string{"a", "x"}),
		"another value":                  secretDigest([]byte("key"), map[string]string{"a": "bc", "x": "2"}, []string{"a", "x"}),
		"the same bytes split elsewhere": secretDigest([]byte("key"), values, []string{"ab", "x"}),
	} {
		if other == digest {
			t.Errorf("%s gave the same digest", what)
		}
	}
	if strings.Contains(digest, "bc") || len(digest) != 64 {
		t.Errorf("digest %q", digest)
	}
}

func TestComposePsOutputIsRead(t *testing.T) {
	containers, err := parsePs(`{"Service":"app","State":"running","Health":"healthy","Status":"Up 1 minute"}` + "\n" +
		`{"Service":"db","State":"exited","Health":"","Status":"Exited (1)"}` + "\n")
	if err != nil || len(containers) != 2 || containers[0].Health != "healthy" || containers[1].State != "exited" {
		t.Errorf("%+v, %v", containers, err)
	}
	if _, err := parsePs("not json\n"); err == nil {
		t.Error("garbage was read")
	}
}
