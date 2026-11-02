package agent

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"maps"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/webcodingcat/clowder/internal/clusterspec"
	"github.com/webcodingcat/clowder/internal/compose"
	"github.com/webcodingcat/clowder/internal/datakey"
	"github.com/webcodingcat/clowder/internal/docker"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/resource"
	"github.com/webcodingcat/clowder/internal/stackspec"
	"github.com/webcodingcat/clowder/internal/state"
)

// testCluster is a state that applies what is proposed, as the leader
// would; the runner's host is host 0.
type testCluster struct {
	t     *testing.T
	mu    sync.Mutex
	state *state.State
	keys  *datakey.Key
}

func (c *testCluster) Read(use func(current *state.State)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	use(c.state)
}

func (c *testCluster) SharedCopy() (*state.State, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state.Copy()
}

func (c *testCluster) Changed() <-chan struct{} { return make(chan struct{}) }

func (c *testCluster) propose(_ context.Context, cmd state.Command) (uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cmd.At = time.Now()
	index := c.state.Index + 1
	_, err := c.state.Apply(index, cmd)
	return index, err
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

// put applies a resource of the namespace default, as clowder apply does.
func (c *testCluster) put(kind, name string, spec any) {
	c.t.Helper()
	encoded, err := json.Marshal(spec)
	if err != nil {
		c.t.Fatal(err)
	}
	c.apply(state.KindPutResource, state.UserActor("alice"), state.PutResource{
		Kind: kind, Namespace: names.Default, Name: name, Owner: state.OwnerCLI, Spec: encoded,
	})
}

// remove deletes a resource of the namespace default.
func (c *testCluster) remove(kind, name string) {
	c.t.Helper()
	c.apply(state.KindDeleteResource, state.UserActor("alice"), state.DeleteResource{
		Kind: kind, Namespace: names.Default, Name: name, Owner: state.OwnerCLI,
	})
}

// putSecret applies a secret of the namespace default, sealed with the
// cluster's data key; its digest stands for one the API computes.
func (c *testCluster) putSecret(name, value string) {
	c.t.Helper()
	sealed, err := c.keys.Seal([]byte(value))
	if err != nil {
		c.t.Fatal(err)
	}
	c.put(resource.KindSecret, name, state.SealedSecret{Value: sealed, Digest: "digest of " + value})
}

// place puts a stack of the namespace default on host 0, as the placement
// does.
func (c *testCluster) place(name string) {
	c.t.Helper()
	host := state.HostID(0)
	c.apply(state.KindSetStackStatus, state.LeaderActor, state.SetStackStatus{
		ID: names.ID(names.Default, name), Status: state.StackStarting, Host: &host,
	})
}

// stack returns a stack of the namespace default.
func (c *testCluster) stack(name string) state.Stack {
	c.mu.Lock()
	defer c.mu.Unlock()
	return *c.state.Stacks[names.ID(names.Default, name)]
}

// shopProject is the Compose project of default/shop, the name of all the
// runner keeps of it.
const shopProject = "default_shop"

// dbLV is the LV of the volume default/db in a data generation.
func dbLV(generation int64) string {
	return names.VolumeName(names.ID(names.Default, "db"), generation)
}

// usingDB is a stack whose service app mounts the volume db.
func usingDB(app stackspec.Service) stackspec.Spec {
	app.Image = "nginx"
	app.Volumes = []stackspec.Mount{{Volume: "db", Path: "/data"}}
	return stackspec.Spec{Services: map[string]stackspec.Service{"app": app}}
}

// newTestRunner is a runner on host 0 of a cluster with the stack shop of
// the namespace default: one service that mounts the volume db (data
// generation 1), both placed on host 0.
func newTestRunner(t *testing.T) (*stackRunner, *testCluster, *fakeDocker, *fakeStorage) {
	t.Helper()
	keys, err := datakey.New()
	if err != nil {
		t.Fatal(err)
	}
	cluster := &testCluster{t: t, state: &state.State{}, keys: keys}
	cluster.apply(state.KindBootstrap, state.LeaderActor, state.Bootstrap{
		Keys:    state.ClusterKeys{CACertificate: []byte("ca"), CAKey: state.Sealed("k"), AgeIdentity: state.Sealed("a")},
		JoinKey: state.SealedSecret{Value: state.Sealed("j"), Digest: "d"},
		Host: state.Host{
			ID: 0, Name: "node0", Underlay: netip.MustParseAddr("10.0.0.2"), WGPublicKey: "wg", CertificateSerial: "s0",
			CertificateNotAfter: time.Now().AddDate(1, 0, 0),
		},
	})
	cluster.apply(state.KindApplyConfig, state.UserActor("alice"), state.ApplyConfig{
		Spec: clusterspec.Config{
			Name: "prod", Seeds: []string{"10.0.0.2"},
			Hoster:         clusterspec.Hoster{Type: "hetzner", Token: "hoster.token"},
			Ingress:        clusterspec.Ingress{FloatingIP: "203.0.113.10"},
			StorageClasses: map[string]clusterspec.StorageClass{"fast": {Pool: "vg-fast/thin"}},
		},
		Secrets: map[string]state.SealedSecret{"hoster.token": {Value: state.Sealed("t"), Digest: "d"}},
	})
	cluster.put(resource.KindVolume, "db", resource.VolumeSpec{Class: "fast", Size: 1 << 30, Replicas: 0})
	cluster.put(resource.KindStack, "shop", usingDB(stackspec.Service{}))
	placeVolume(cluster, "db", 1)
	cluster.place("shop")

	docker, storage := &fakeDocker{}, &fakeStorage{dir: t.TempDir()}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	runner := &stackRunner{
		files: Files{Dir: t.TempDir(), SecretsDir: t.TempDir()}, host: Host{ID: 0, Name: "node0", NodeAddress: netip.MustParseAddr("10.77.0.1")},
		cluster: cluster, docker: docker, storage: storage, supervisor: fakePauser{&docker.calls}, signal: newPassSignal(), logger: logger,
		propose: cluster.propose, applied: map[string]broughtUp{}, logins: map[string][32]byte{},
		unexplained: map[string]bool{}, open: keys.Open, digestKey: []byte("the test's digest key"),
	}
	return runner, cluster, docker, storage
}

// fakePauser stands for the supervisor: it adds each pause and resume to
// Docker's calls, so a test sees what ran between them.
type fakePauser struct {
	calls *calls
}

func (p fakePauser) Pause(project string) func() {
	p.calls.add("pause %s", project)
	return func() { p.calls.add("resume %s", project) }
}

// placeVolume places a volume of the namespace default on host 0 in a data
// generation, as the placement (the first) or a restore (the next) does.
func placeVolume(cluster *testCluster, name string, generation int64) {
	cluster.t.Helper()
	var volume state.Volume
	cluster.Read(func(current *state.State) { volume = *current.Volumes[names.ID(names.Default, name)] })
	volume.DataGeneration, volume.Status = generation, state.VolumeOK
	volume.Copies = []state.VolumeCopy{{Host: 0, Class: "fast"}}
	cluster.apply(state.KindPutVolume, state.LeaderActor, state.PutVolume{Volume: volume})
}

func (c *calls) count(line string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(slices.DeleteFunc(slices.Clone(c.lines), func(l string) bool { return l != line }))
}

func TestAFailedComposeUpIsTriedAgain(t *testing.T) {
	runner, cluster, docker, _ := newTestRunner(t)
	docker.failedUps = 2
	runner.pass(context.Background())
	if stack := cluster.stack("shop"); stack.Status != state.StackFailing || stack.UpError != "pull access denied" {
		t.Errorf("after a failed compose up the stack is %s: %q", stack.Status, stack.UpError)
	}
	version := cluster.stack("shop").ResourceVersion
	runner.pass(context.Background()) // fails again: nothing new to report
	if stack := cluster.stack("shop"); stack.ResourceVersion != version {
		t.Errorf("a second failure was reported again: %s, %q", stack.Status, stack.UpError)
	}
	runner.pass(context.Background())
	if ups := docker.count("up " + shopProject); ups != 3 {
		t.Errorf("%d compose ups, want 3", ups)
	}
	if stack := cluster.stack("shop"); stack.UpError != "" {
		t.Errorf("the error stayed: %q", stack.UpError)
	}
	if status := cluster.stack("shop").Status; status != state.StackRunning {
		t.Errorf("after compose up the stack is %s", status)
	}
	runner.pass(context.Background()) // nothing changed: no compose up
	if ups := docker.count("up " + shopProject); ups != 3 {
		t.Errorf("%d compose ups without a change, want 3", ups)
	}
	runner.signal.dockerRestarted()
	runner.pass(context.Background())
	if ups := docker.count("up " + shopProject); ups != 4 {
		t.Errorf("%d compose ups after Docker restarted, want 4", ups)
	}
}

func TestComposeChangesContainersOnlyWhileTheSupervisorIsPaused(t *testing.T) {
	runner, cluster, docker, _ := newTestRunner(t)
	runner.pass(context.Background())
	cluster.remove(resource.KindStack, "shop")
	runner.pass(context.Background())
	docker.mu.Lock()
	defer docker.mu.Unlock()
	want := []string{
		"pause " + shopProject, "up " + shopProject, "resume " + shopProject,
		"pause " + shopProject, "down " + shopProject, "resume " + shopProject,
	}
	if !slices.Equal(docker.lines, want) {
		t.Errorf("docker %q, want %q", docker.lines, want)
	}
}

func TestAStackThatFailsAfterARestartOfDockerIsReportedFailing(t *testing.T) {
	runner, cluster, docker, _ := newTestRunner(t)
	cluster.putSecret("db-password", "first")
	cluster.put(resource.KindStack, "shop", usingDB(stackspec.Service{Secrets: []string{"db-password"}}))
	runner.pass(context.Background())
	applied := cluster.stack("shop").AppliedSecrets

	docker.failedUps = 1
	runner.signal.dockerRestarted()
	runner.pass(context.Background())
	stack := cluster.stack("shop")
	if stack.Status != state.StackFailing || !maps.Equal(stack.AppliedSecrets, applied) {
		t.Errorf("the stack is %s with the secrets %v, want failing with %v", stack.Status, stack.AppliedSecrets, applied)
	}
	runner.pass(context.Background())
	if stack := cluster.stack("shop"); stack.Status != state.StackRunning || stack.UpError != "" {
		t.Errorf("once up, the stack is %s: %q", stack.Status, stack.UpError)
	}
}

func TestAFailingStackIsStartingAgainWithANewGeneration(t *testing.T) {
	runner, cluster, docker, _ := newTestRunner(t)
	docker.failedUps = 1
	runner.pass(context.Background())
	cluster.put(resource.KindStack, "shop", usingDB(stackspec.Service{Command: []string{"serve"}}))
	if stack := cluster.stack("shop"); stack.Status != state.StackStarting || stack.UpError != "" {
		t.Errorf("a failing stack applied anew is %s: %q", stack.Status, stack.UpError)
	}
}

// shopContainer is the container of shop's service app as Docker lists it,
// with the restart time it came up with ("" for none).
func shopContainer(restarted string) []docker.Container {
	labels := map[string]string{compose.LabelStack: shopProject, docker.ServiceLabel: "app"}
	if restarted != "" {
		labels[compose.LabelRestarted] = restarted
	}
	return []docker.Container{{ID: "app-1", State: "running", Labels: labels}}
}

func TestARestartPullsAndRecreatesOnce(t *testing.T) {
	runner, cluster, docker, _ := newTestRunner(t)
	runner.pass(context.Background())
	docker.mu.Lock()
	docker.containers = shopContainer("") // as compose up left it
	docker.mu.Unlock()

	cluster.apply(state.KindRestartStack, state.UserActor("alice"), state.RestartStack{Namespace: "default", Name: "shop"})
	if status := cluster.stack("shop").Status; status != state.StackStarting {
		t.Errorf("a restarted stack is %s", status)
	}
	runner.pass(context.Background())
	if pulls, ups := docker.count("pull "+shopProject+" app"), docker.count("up "+shopProject); pulls != 1 || ups != 2 {
		t.Errorf("%d pulls and %d compose ups after a restart, want 1 and 2", pulls, ups)
	}
	content, err := os.ReadFile(runner.files.stackFile(shopProject))
	restarted := compose.RestartedLabel(cluster.stack("shop").Restarted["app"])
	if err != nil || !strings.Contains(string(content), restarted) {
		t.Errorf("the rendered file lacks the restart's label %s: %v", restarted, err)
	}
	if status := cluster.stack("shop").Status; status != state.StackRunning {
		t.Errorf("after the restart the stack is %s", status)
	}

	// the container came up with the label: after Docker restarted, compose
	// up runs again but pulls nothing
	docker.mu.Lock()
	docker.containers = shopContainer(restarted)
	docker.mu.Unlock()
	runner.signal.dockerRestarted()
	runner.pass(context.Background())
	if pulls, ups := docker.count("pull "+shopProject+" app"), docker.count("up "+shopProject); pulls != 1 || ups != 3 {
		t.Errorf("%d pulls and %d compose ups after Docker restarted, want 1 and 3", pulls, ups)
	}
}

func TestAComposeUpThatDockersRestartOvertookIsRunAgain(t *testing.T) {
	runner, _, docker, _ := newTestRunner(t)
	docker.duringUp = func() {
		docker.duringUp = nil // once
		runner.signal.dockerRestarted()
	}
	runner.pass(context.Background())
	runner.pass(context.Background()) // the pass the restart asked for
	if ups := docker.count("up " + shopProject); ups != 2 {
		t.Errorf("%d compose ups, want 2: the first may have come up before Docker restarted", ups)
	}
}

func TestAPushedOutServiceIsLeftOutAndNotRestarted(t *testing.T) {
	runner, cluster, docker, _ := newTestRunner(t)
	runner.pass(context.Background())
	rules := restartRules{cluster, 0}
	if rule, ok := rules.of(shopProject, "app"); !ok || rule.Restart != "always" || !rule.RestartUnhealthy {
		t.Errorf("the rule of a service %+v, %v", rule, ok)
	}
	cluster.mu.Lock()
	cluster.state.Stacks["default/shop"].PushedOut = []string{"app"} // as set-pushed-out writes it
	cluster.mu.Unlock()
	runner.pass(context.Background())
	docker.mu.Lock()
	leftOut := docker.leftOut
	docker.mu.Unlock()
	if ups := docker.count("up " + shopProject); ups != 2 || !slices.Equal(leftOut, []string{"app"}) {
		t.Errorf("%d compose ups, the last without %v; want 2, without app", ups, leftOut)
	}
	if rule, ok := rules.of(shopProject, "app"); !ok || rule.Restart != "no" {
		t.Errorf("the rule of a pushed-out service %+v, %v", rule, ok)
	}
}

func TestANewDataGenerationReplacesTheLV(t *testing.T) {
	runner, cluster, docker, storage := newTestRunner(t)
	runner.pass(context.Background())
	placeVolume(cluster, "db", 2) // a restore
	runner.pass(context.Background())
	old, current := dbLV(1), dbLV(2)
	if storage.count("ensure "+current) != 1 || storage.count("remove "+old) != 1 || storage.count("remove "+current) != 0 {
		t.Errorf("storage calls %q", storage.lines)
	}
	if ups := docker.count("up " + shopProject); ups != 2 {
		t.Errorf("%d compose ups, want 2 (the new volume)", ups)
	}
	if retired := cluster.state.Retired; len(retired) != 0 {
		t.Errorf("the removed copy is not forgotten: %v", retired)
	}
}

func TestACopyTheStateLostIsKept(t *testing.T) {
	runner, cluster, _, storage := newTestRunner(t)
	runner.pass(context.Background())
	// a bug, not a command, takes the volume and its stack out of the state
	cluster.mu.Lock()
	delete(cluster.state.Volumes, "default/db")
	delete(cluster.state.Stacks, "default/shop")
	cluster.mu.Unlock()
	runner.pass(context.Background())
	runner.pass(context.Background())
	if storage.count("remove "+dbLV(1)) != 0 {
		t.Errorf("a copy that is neither placed nor retired was removed: %q", storage.lines)
	}
	if !runner.unexplained[dbLV(1)] {
		t.Error("no warning about the kept copy")
	}
}

func TestAVolumeOutlivesItsStack(t *testing.T) {
	runner, cluster, docker, storage := newTestRunner(t)
	runner.pass(context.Background())

	cluster.remove(resource.KindStack, "shop")
	runner.pass(context.Background())
	runner.pass(context.Background())
	if docker.count("down "+shopProject) != 1 || storage.count("remove "+dbLV(1)) != 0 {
		t.Errorf("docker %q, storage %q", docker.lines, storage.lines)
	}
	// the stack's files go with compose down, the copy's record stays
	if _, err := os.Stat(runner.files.stackDir(shopProject)); !os.IsNotExist(err) {
		t.Errorf("the stack's directory is left: %v", err)
	}
	if _, err := os.Stat(runner.files.volumeRecord("default/db")); err != nil {
		t.Errorf("the record of the copy left with the stack: %v", err)
	}

	cluster.remove(resource.KindVolume, "db")
	runner.pass(context.Background())
	if storage.count("remove "+dbLV(1)) != 1 {
		t.Errorf("storage %q", storage.lines)
	}
	if retired := cluster.state.Retired; len(retired) != 0 {
		t.Errorf("the removed copy is not forgotten: %v", retired)
	}
	if _, err := os.Stat(runner.files.volumeRecord("default/db")); !os.IsNotExist(err) {
		t.Errorf("the record of the removed copy is left: %v", err)
	}
}

func TestSecretsReachTheHostAndLeaveWithTheStack(t *testing.T) {
	runner, cluster, docker, _ := newTestRunner(t)
	password := runner.files.secretFile(shopProject, "db-password")
	env := runner.files.secretEnvFile(shopProject)

	cluster.putSecret("db-password", "first")
	cluster.putSecret("api-key", "key")
	cluster.put(resource.KindStack, "shop", usingDB(stackspec.Service{
		Secrets: []string{"db-password"}, SecretEnv: map[string]string{"API_KEY": "api-key"},
	}))
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
	want := map[string]string{"db-password": "digest of first", "api-key": "digest of key"}
	if applied := cluster.stack("shop").AppliedSecrets; !maps.Equal(applied, want) {
		t.Errorf("applied secrets %v, want %v", applied, want)
	}

	// a new password, the stack unchanged: rewritten, and reported
	cluster.putSecret("db-password", "second")
	runner.pass(context.Background())
	if content, err := os.ReadFile(password); err != nil || string(content) != "second" {
		t.Errorf("the changed secret file: %q, %v", content, err)
	}
	// the file keeps its path: only the label makes compose recreate app
	if secretsLabel(t, runner) == firstLabel {
		t.Error("the label of app did not change with its secret file")
	}
	if applied := cluster.stack("shop").AppliedSecrets; applied["db-password"] != "digest of second" {
		t.Errorf("applied secrets %v after the change", applied)
	}

	// no variable any more: env.yaml goes
	cluster.put(resource.KindStack, "shop", usingDB(stackspec.Service{Secrets: []string{"db-password"}}))
	runner.pass(context.Background())
	if _, err := os.Stat(env); !os.IsNotExist(err) {
		t.Errorf("the secret variables stayed: %v", err)
	}
	if ups := docker.count("up " + shopProject); ups != 3 || len(docker.upFiles) != 1 {
		t.Errorf("%d compose ups, the last with %v", ups, docker.upFiles)
	}

	cluster.apply(state.KindSetStackStatus, state.LeaderActor, state.SetStackStatus{ID: "default/shop", Status: state.StackPending})
	runner.pass(context.Background())
	if _, err := os.Stat(runner.files.secretsDir(shopProject)); !os.IsNotExist(err) {
		t.Errorf("the secrets of a stack that left stayed: %v", err)
	}
}

// secretsLabel reads the digest label of app from shop's rendered file.
func secretsLabel(t *testing.T, runner *stackRunner) string {
	t.Helper()
	content, err := os.ReadFile(runner.files.stackFile(shopProject))
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
