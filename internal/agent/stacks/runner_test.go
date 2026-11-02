package stacks

import (
	"context"
	"io"
	"log/slog"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/webcodingcat/clowder/internal/agent/agenttest"
	"github.com/webcodingcat/clowder/internal/agent/loop"
	"github.com/webcodingcat/clowder/internal/compose"
	"github.com/webcodingcat/clowder/internal/docker"
	"github.com/webcodingcat/clowder/internal/resource"
	"github.com/webcodingcat/clowder/internal/stackspec"
	"github.com/webcodingcat/clowder/internal/state"
)

// newTestRunner is a runner on host 0 of agenttest.NewCluster: the stack
// shop of the namespace default, with one service that mounts the volume
// db (data generation 1), both placed on host 0.
func newTestRunner(t *testing.T) (*Runner, *agenttest.Cluster, *agenttest.Docker, *agenttest.Storage) {
	t.Helper()
	cluster := agenttest.NewCluster(t)
	docker, storage := &agenttest.Docker{}, &agenttest.Storage{Dir: t.TempDir()}
	dir := t.TempDir()
	runner := New(Parts{
		Files: Files{
			StacksDir: filepath.Join(dir, "stacks"), VolumeRecordsDir: filepath.Join(dir, "volume-records"),
			SecretsDir: t.TempDir(), LastTrim: filepath.Join(dir, "last-trim"),
		},
		Host: 0, NodeAddress: netip.MustParseAddr("10.77.0.1"), Cluster: cluster, Docker: docker, Storage: storage,
		Open: cluster.Keys.Open, DigestKey: []byte("the test's digest key"), Supervisor: fakePauser{&docker.Calls},
		Signal: loop.NewSignal(), Propose: cluster.Propose, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return runner, cluster, docker, storage
}

// fakePauser stands for the supervisor: it adds each pause and resume to
// Docker's calls, so a test sees what ran between them.
type fakePauser struct {
	calls *agenttest.Calls
}

func (p fakePauser) Pause(project string) func() {
	p.calls.Add("pause %s", project)
	return func() { p.calls.Add("resume %s", project) }
}

const shopProject = agenttest.ShopProject

func TestAFailedComposeUpIsTriedAgain(t *testing.T) {
	runner, cluster, docker, _ := newTestRunner(t)
	docker.FailedUps = 2
	runner.pass(context.Background())
	if stack := cluster.Stack("shop"); stack.Status != state.StackFailing || stack.UpError != "pull access denied" {
		t.Errorf("after a failed compose up the stack is %s: %q", stack.Status, stack.UpError)
	}
	version := cluster.Stack("shop").ResourceVersion
	runner.pass(context.Background()) // fails again: nothing new to report
	if stack := cluster.Stack("shop"); stack.ResourceVersion != version {
		t.Errorf("a second failure was reported again: %s, %q", stack.Status, stack.UpError)
	}
	runner.pass(context.Background())
	if ups := docker.Count("up " + shopProject); ups != 3 {
		t.Errorf("%d compose ups, want 3", ups)
	}
	if stack := cluster.Stack("shop"); stack.UpError != "" {
		t.Errorf("the error stayed: %q", stack.UpError)
	}
	if status := cluster.Stack("shop").Status; status != state.StackRunning {
		t.Errorf("after compose up the stack is %s", status)
	}
	runner.pass(context.Background()) // nothing changed: no compose up
	if ups := docker.Count("up " + shopProject); ups != 3 {
		t.Errorf("%d compose ups without a change, want 3", ups)
	}
	runner.signal.DockerRestarted()
	runner.pass(context.Background())
	if ups := docker.Count("up " + shopProject); ups != 4 {
		t.Errorf("%d compose ups after Docker restarted, want 4", ups)
	}
}

func TestComposeChangesContainersOnlyWhileTheSupervisorIsPaused(t *testing.T) {
	runner, cluster, docker, _ := newTestRunner(t)
	runner.pass(context.Background())
	cluster.Remove(resource.KindStack, "shop")
	runner.pass(context.Background())
	want := []string{
		"pause " + shopProject, "up " + shopProject, "resume " + shopProject,
		"pause " + shopProject, "down " + shopProject, "resume " + shopProject,
	}
	if got := docker.Lines(); !slices.Equal(got, want) {
		t.Errorf("docker %q, want %q", got, want)
	}
}

func TestAStackThatFailsAfterARestartOfDockerIsReportedFailing(t *testing.T) {
	runner, cluster, docker, _ := newTestRunner(t)
	cluster.PutSecret("db-password", "first")
	cluster.Put(resource.KindStack, "shop", agenttest.UsingDB(stackspec.Service{Secrets: []string{"db-password"}}))
	runner.pass(context.Background())
	applied := cluster.Stack("shop").AppliedSecrets

	docker.FailedUps = 1
	runner.signal.DockerRestarted()
	runner.pass(context.Background())
	stack := cluster.Stack("shop")
	if stack.Status != state.StackFailing || !maps.Equal(stack.AppliedSecrets, applied) {
		t.Errorf("the stack is %s with the secrets %v, want failing with %v", stack.Status, stack.AppliedSecrets, applied)
	}
	runner.pass(context.Background())
	if stack := cluster.Stack("shop"); stack.Status != state.StackRunning || stack.UpError != "" {
		t.Errorf("once up, the stack is %s: %q", stack.Status, stack.UpError)
	}
}

func TestAFailingStackIsStartingAgainWithANewGeneration(t *testing.T) {
	runner, cluster, docker, _ := newTestRunner(t)
	docker.FailedUps = 1
	runner.pass(context.Background())
	cluster.Put(resource.KindStack, "shop", agenttest.UsingDB(stackspec.Service{Command: []string{"serve"}}))
	if stack := cluster.Stack("shop"); stack.Status != state.StackStarting || stack.UpError != "" {
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
	docker.Lock()
	docker.Listed = shopContainer("") // as compose up left it
	docker.Unlock()

	cluster.Apply(state.KindRestartStack, state.UserActor("alice"), state.RestartStack{Namespace: "default", Name: "shop"})
	if status := cluster.Stack("shop").Status; status != state.StackStarting {
		t.Errorf("a restarted stack is %s", status)
	}
	runner.pass(context.Background())
	if pulls, ups := docker.Count("pull "+shopProject+" app"), docker.Count("up "+shopProject); pulls != 1 || ups != 2 {
		t.Errorf("%d pulls and %d compose ups after a restart, want 1 and 2", pulls, ups)
	}
	content, err := os.ReadFile(runner.files.stackFile(shopProject))
	restarted := compose.RestartedLabel(cluster.Stack("shop").Restarted["app"])
	if err != nil || !strings.Contains(string(content), restarted) {
		t.Errorf("the rendered file lacks the restart's label %s: %v", restarted, err)
	}
	if status := cluster.Stack("shop").Status; status != state.StackRunning {
		t.Errorf("after the restart the stack is %s", status)
	}

	// the container came up with the label: after Docker restarted, compose
	// up runs again but pulls nothing
	docker.Lock()
	docker.Listed = shopContainer(restarted)
	docker.Unlock()
	runner.signal.DockerRestarted()
	runner.pass(context.Background())
	if pulls, ups := docker.Count("pull "+shopProject+" app"), docker.Count("up "+shopProject); pulls != 1 || ups != 3 {
		t.Errorf("%d pulls and %d compose ups after Docker restarted, want 1 and 3", pulls, ups)
	}
}

func TestAComposeUpThatDockersRestartOvertookIsRunAgain(t *testing.T) {
	runner, _, docker, _ := newTestRunner(t)
	docker.DuringUp = func() {
		docker.DuringUp = nil // once
		runner.signal.DockerRestarted()
	}
	runner.pass(context.Background())
	runner.pass(context.Background()) // the pass the restart asked for
	if ups := docker.Count("up " + shopProject); ups != 2 {
		t.Errorf("%d compose ups, want 2: the first may have come up before Docker restarted", ups)
	}
}

func TestAPushedOutServiceIsLeftOut(t *testing.T) {
	runner, cluster, docker, _ := newTestRunner(t)
	runner.pass(context.Background())
	cluster.Lock()
	cluster.State.Stacks["default/shop"].PushedOut = []string{"app"} // as set-pushed-out writes it
	cluster.Unlock()
	runner.pass(context.Background())
	docker.Lock()
	leftOut := docker.LeftOut
	docker.Unlock()
	if ups := docker.Count("up " + shopProject); ups != 2 || !slices.Equal(leftOut, []string{"app"}) {
		t.Errorf("%d compose ups, the last without %v; want 2, without app", ups, leftOut)
	}
}

func TestANewDataGenerationReplacesTheLV(t *testing.T) {
	runner, cluster, docker, storage := newTestRunner(t)
	runner.pass(context.Background())
	cluster.PlaceVolume("db", 2) // a restore
	runner.pass(context.Background())
	old, current := agenttest.DBLV(1), agenttest.DBLV(2)
	if storage.Count("ensure "+current) != 1 || storage.Count("remove "+old) != 1 || storage.Count("remove "+current) != 0 {
		t.Errorf("storage calls %q", storage.Lines())
	}
	if ups := docker.Count("up " + shopProject); ups != 2 {
		t.Errorf("%d compose ups, want 2 (the new volume)", ups)
	}
	if retired := cluster.State.Retired; len(retired) != 0 {
		t.Errorf("the removed copy is not forgotten: %v", retired)
	}
}

func TestACopyTheStateLostIsKept(t *testing.T) {
	runner, cluster, _, storage := newTestRunner(t)
	runner.pass(context.Background())
	// a bug, not a command, takes the volume and its stack out of the state
	cluster.Lock()
	delete(cluster.State.Volumes, "default/db")
	delete(cluster.State.Stacks, "default/shop")
	cluster.Unlock()
	runner.pass(context.Background())
	runner.pass(context.Background())
	if storage.Count("remove "+agenttest.DBLV(1)) != 0 {
		t.Errorf("a copy that is neither placed nor retired was removed: %q", storage.Lines())
	}
	if !runner.unexplained[agenttest.DBLV(1)] {
		t.Error("no warning about the kept copy")
	}
}

func TestAVolumeOutlivesItsStack(t *testing.T) {
	runner, cluster, docker, storage := newTestRunner(t)
	runner.pass(context.Background())

	cluster.Remove(resource.KindStack, "shop")
	runner.pass(context.Background())
	runner.pass(context.Background())
	if docker.Count("down "+shopProject) != 1 || storage.Count("remove "+agenttest.DBLV(1)) != 0 {
		t.Errorf("docker %q, storage %q", docker.Lines(), storage.Lines())
	}
	// the stack's files go with compose down, the copy's record stays
	if _, err := os.Stat(runner.files.stackDir(shopProject)); !os.IsNotExist(err) {
		t.Errorf("the stack's directory is left: %v", err)
	}
	if _, err := os.Stat(runner.files.volumeRecord("default/db")); err != nil {
		t.Errorf("the record of the copy left with the stack: %v", err)
	}

	cluster.Remove(resource.KindVolume, "db")
	runner.pass(context.Background())
	if storage.Count("remove "+agenttest.DBLV(1)) != 1 {
		t.Errorf("storage %q", storage.Lines())
	}
	if retired := cluster.State.Retired; len(retired) != 0 {
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

	cluster.PutSecret("db-password", "first")
	cluster.PutSecret("api-key", "key")
	cluster.Put(resource.KindStack, "shop", agenttest.UsingDB(stackspec.Service{
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
	if len(docker.UpFiles) != 2 || docker.UpFiles[1] != env {
		t.Errorf("compose up with %v", docker.UpFiles)
	}
	want := map[string]string{"db-password": "digest of first", "api-key": "digest of key"}
	if applied := cluster.Stack("shop").AppliedSecrets; !maps.Equal(applied, want) {
		t.Errorf("applied secrets %v, want %v", applied, want)
	}

	// a new password, the stack unchanged: rewritten, and reported
	cluster.PutSecret("db-password", "second")
	runner.pass(context.Background())
	if content, err := os.ReadFile(password); err != nil || string(content) != "second" {
		t.Errorf("the changed secret file: %q, %v", content, err)
	}
	// the file keeps its path: only the label makes compose recreate app
	if secretsLabel(t, runner) == firstLabel {
		t.Error("the label of app did not change with its secret file")
	}
	if applied := cluster.Stack("shop").AppliedSecrets; applied["db-password"] != "digest of second" {
		t.Errorf("applied secrets %v after the change", applied)
	}

	// no variable any more: env.yaml goes
	cluster.Put(resource.KindStack, "shop", agenttest.UsingDB(stackspec.Service{Secrets: []string{"db-password"}}))
	runner.pass(context.Background())
	if _, err := os.Stat(env); !os.IsNotExist(err) {
		t.Errorf("the secret variables stayed: %v", err)
	}
	if ups := docker.Count("up " + shopProject); ups != 3 || len(docker.UpFiles) != 1 {
		t.Errorf("%d compose ups, the last with %v", ups, docker.UpFiles)
	}

	cluster.Apply(state.KindSetStackStatus, state.LeaderActor, state.SetStackStatus{ID: "default/shop", Status: state.StackPending})
	runner.pass(context.Background())
	if _, err := os.Stat(runner.files.secretsDir(shopProject)); !os.IsNotExist(err) {
		t.Errorf("the secrets of a stack that left stayed: %v", err)
	}
}

// secretsLabel reads the digest label of app from shop's rendered file.
func secretsLabel(t *testing.T, runner *Runner) string {
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
