package stacks

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/webcodingcat/clowder/internal/agent/agenttest"
	"github.com/webcodingcat/clowder/internal/agent/loop"
	"github.com/webcodingcat/clowder/internal/compose"
	"github.com/webcodingcat/clowder/internal/docker"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/resource"
	"github.com/webcodingcat/clowder/internal/stackspec"
	"github.com/webcodingcat/clowder/internal/state"
)

// newTestRunner is a runner on host 0 of agenttest.NewCluster: the stack
// shop of the namespace default, with one service that mounts the volume
// db (data generation 1), both placed on host 0.
func newTestRunner(t *testing.T) (*Runner, *agenttest.Cluster, *agenttest.Docker, *agenttest.Volumes) {
	t.Helper()
	cluster := agenttest.NewCluster(t)
	docker, volumes := &agenttest.Docker{}, &agenttest.Volumes{Dir: t.TempDir()}
	files := Files{StacksDir: filepath.Join(t.TempDir(), "stacks"), SecretsDir: t.TempDir()}
	runner := New(Parts{
		Files:       files,
		Host:        0,
		NodeAddress: netip.MustParseAddr("10.77.0.1"), Cluster: cluster, Docker: docker, Volumes: volumes,
		OpenSecret: cluster.OpenSecret, Registries: NewRegistryConfigs(files, cluster.OpenSecret),
		Supervisor: fakePauser{&docker.Calls},
		Signal:     loop.NewSignal(), Propose: cluster.Propose, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return runner, cluster, docker, volumes
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
	runner.passAndWait(context.Background())
	if stack := cluster.Stack("shop"); stack.Status != state.StackFailing || stack.UpError != "pull access denied" {
		t.Errorf("after a failed compose up the stack is %s: %q", stack.Status, stack.UpError)
	}
	version := cluster.Stack("shop").ResourceVersion
	runner.passAndWait(context.Background()) // fails again: nothing new to report
	if stack := cluster.Stack("shop"); stack.ResourceVersion != version {
		t.Errorf("a second failure was reported again: %s, %q", stack.Status, stack.UpError)
	}
	runner.passAndWait(context.Background())
	if ups := docker.Count("up " + shopProject); ups != 3 {
		t.Errorf("%d compose ups, want 3", ups)
	}
	if stack := cluster.Stack("shop"); stack.UpError != "" {
		t.Errorf("the error stayed: %q", stack.UpError)
	}
	if status := cluster.Stack("shop").Status; status != state.StackRunning {
		t.Errorf("after compose up the stack is %s", status)
	}
	runner.passAndWait(context.Background()) // nothing changed: no compose up
	if ups := docker.Count("up " + shopProject); ups != 3 {
		t.Errorf("%d compose ups without a change, want 3", ups)
	}
	runner.signal.DockerRestarted()
	runner.passAndWait(context.Background())
	if ups := docker.Count("up " + shopProject); ups != 4 {
		t.Errorf("%d compose ups after Docker restarted, want 4", ups)
	}
}

func TestComposeChangesContainersOnlyWhileTheSupervisorIsPaused(t *testing.T) {
	runner, cluster, docker, _ := newTestRunner(t)
	runner.passAndWait(context.Background())
	cluster.Remove(resource.KindStack, "shop")
	runner.passAndWait(context.Background())
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
	runner.passAndWait(context.Background())
	applied := cluster.Stack("shop").AppliedSecrets

	docker.FailedUps = 1
	runner.signal.DockerRestarted()
	runner.passAndWait(context.Background())
	stack := cluster.Stack("shop")
	if stack.Status != state.StackFailing || !maps.Equal(stack.AppliedSecrets, applied) {
		t.Errorf("the stack is %s with the secrets %v, want failing with %v", stack.Status, stack.AppliedSecrets, applied)
	}
	runner.passAndWait(context.Background())
	if stack := cluster.Stack("shop"); stack.Status != state.StackRunning || stack.UpError != "" {
		t.Errorf("once up, the stack is %s: %q", stack.Status, stack.UpError)
	}
}

func TestAFailingStackIsStartingAgainWithANewGeneration(t *testing.T) {
	runner, cluster, docker, _ := newTestRunner(t)
	docker.FailedUps = 1
	runner.passAndWait(context.Background())
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
	runner.passAndWait(context.Background())
	docker.Lock()
	docker.Listed = shopContainer("") // as compose up left it
	docker.Unlock()

	cluster.Apply(state.KindRestartStack, state.CLIActor("admin"), state.RestartStack{Namespace: "default", Name: "shop"})
	if status := cluster.Stack("shop").Status; status != state.StackStarting {
		t.Errorf("a restarted stack is %s", status)
	}
	runner.passAndWait(context.Background())
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
	runner.passAndWait(context.Background())
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
	runner.passAndWait(context.Background())
	runner.passAndWait(context.Background()) // the pass the restart asked for
	if ups := docker.Count("up " + shopProject); ups != 2 {
		t.Errorf("%d compose ups, want 2: the first may have come up before Docker restarted", ups)
	}
}

func TestAPushedOutServiceIsLeftOut(t *testing.T) {
	runner, cluster, docker, _ := newTestRunner(t)
	runner.passAndWait(context.Background())
	cluster.Lock()
	cluster.State.Stacks["default/shop"].PushedOut = []string{"app"} // as set-pushed-out writes it
	cluster.Unlock()
	runner.passAndWait(context.Background())
	docker.Lock()
	leftOut := docker.LeftOut
	docker.Unlock()
	if ups := docker.Count("up " + shopProject); ups != 2 || !slices.Equal(leftOut, []string{"app"}) {
		t.Errorf("%d compose ups, the last without %v; want 2, without app", ups, leftOut)
	}
}

func TestANewDataGenerationBringsTheStackUpOnTheNewLV(t *testing.T) {
	runner, cluster, docker, _ := newTestRunner(t)
	runner.passAndWait(context.Background())
	cluster.PlaceVolume("db", 2) // a restore
	runner.passAndWait(context.Background())
	if ups := docker.Count("up " + shopProject); ups != 2 {
		t.Errorf("%d compose ups, want 2 (the new volume)", ups)
	}
	if lvs := mountedLVs(t, runner); !slices.Equal(lvs, []string{agenttest.DBLV(2)}) {
		t.Errorf("the stack holds %v, want only the new LV", lvs)
	}
}

func TestAStackWaitsForItsVolumeAndFailsWithIt(t *testing.T) {
	runner, cluster, docker, volumes := newTestRunner(t)
	lv := agenttest.DBLV(1)
	volumes.Lock()
	volumes.NotReady = map[string]bool{lv: true}
	volumes.Unlock()
	runner.passAndWait(context.Background())
	if docker.Count("up "+shopProject) != 0 || cluster.Stack("shop").Status != state.StackStarting {
		t.Errorf("docker %q, shop %s: a stack whose volume is on its way waits", docker.Lines(), cluster.Stack("shop").Status)
	}

	volumes.Lock()
	volumes.Errors = map[string]error{lv: errors.New("volume default/db holds data, but its only LV is gone")}
	volumes.Unlock()
	runner.passAndWait(context.Background())
	if shop := cluster.Stack("shop"); shop.Status != state.StackFailing || !strings.Contains(shop.UpError, "its only LV is gone") {
		t.Errorf("shop %s (%q): a stack whose volume cannot be mounted fails with why", shop.Status, shop.UpError)
	}

	volumes.Lock()
	volumes.NotReady, volumes.Errors = nil, nil
	volumes.Unlock()
	runner.passAndWait(context.Background())
	if docker.Count("up "+shopProject) != 1 || cluster.Stack("shop").Status != state.StackRunning {
		t.Errorf("docker %q, shop %s", docker.Lines(), cluster.Stack("shop").Status)
	}
}

func TestTheStacksHoldTheLVsOfTheirComposeFiles(t *testing.T) {
	runner, cluster, _, _ := newTestRunner(t)
	runner.passAndWait(context.Background())
	held, err := runner.files.HeldLVs()
	if err != nil || !maps.Equal(held, map[string]bool{agenttest.DBLV(1): true}) {
		t.Errorf("held %v, %v", held, err)
	}

	// once compose down worked, the stack holds nothing
	cluster.Remove(resource.KindStack, "shop")
	runner.passAndWait(context.Background())
	if held, err := runner.files.HeldLVs(); err != nil || len(held) != 0 {
		t.Errorf("held %v, %v after the stack stopped", held, err)
	}

	// a list that does not read may hold any LV
	if err := os.MkdirAll(runner.files.stackDir("default_blog"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runner.files.stackFile("default_blog"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.files.HeldLVs(); err == nil {
		t.Error("a stack without a readable list held nothing")
	}
}

func TestSecretsReachTheHostAndLeaveWithTheStack(t *testing.T) {
	runner, cluster, docker, _ := newTestRunner(t)
	password := runner.files.secretFile(shopProject, "db-password", "value")
	env := runner.files.secretEnvFile(shopProject)

	cluster.PutSecret("db-password", "first")
	cluster.PutSecret("api-key", "key")
	cluster.Put(resource.KindStack, "shop", agenttest.UsingDB(stackspec.Service{
		Secrets: []string{"db-password"}, SecretEnv: map[string]names.SecretKeyRef{"API_KEY": {Name: "api-key", Key: "value"}},
	}))
	runner.passAndWait(context.Background())
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
	want := map[string]string{"db-password": "digest of value=first", "api-key": "digest of value=key"}
	if applied := cluster.Stack("shop").AppliedSecrets; !maps.Equal(applied, want) {
		t.Errorf("applied secrets %v, want %v", applied, want)
	}

	// a new password, the stack unchanged: rewritten, and reported
	cluster.PutSecret("db-password", "second")
	runner.passAndWait(context.Background())
	if content, err := os.ReadFile(password); err != nil || string(content) != "second" {
		t.Errorf("the changed secret file: %q, %v", content, err)
	}
	// the file keeps its path: only the label makes compose recreate app
	if secretsLabel(t, runner) == firstLabel {
		t.Error("the label of app did not change with its secret file")
	}
	if applied := cluster.Stack("shop").AppliedSecrets; applied["db-password"] != "digest of value=second" {
		t.Errorf("applied secrets %v after the change", applied)
	}

	// no variable any more: env.yaml goes
	cluster.Put(resource.KindStack, "shop", agenttest.UsingDB(stackspec.Service{Secrets: []string{"db-password"}}))
	runner.passAndWait(context.Background())
	if _, err := os.Stat(env); !os.IsNotExist(err) {
		t.Errorf("the secret variables stayed: %v", err)
	}
	if ups := docker.Count("up " + shopProject); ups != 3 || len(docker.UpFiles) != 1 {
		t.Errorf("%d compose ups, the last with %v", ups, docker.UpFiles)
	}

	cluster.Apply(state.KindSetStackStatus, state.LeaderActor, state.SetStackStatus{
		ID: "default/shop", Status: state.StackPending, PlannedAt: cluster.Index(),
	})
	runner.passAndWait(context.Background())
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

// passAndWait runs a pass and waits for the jobs it started, as the tests
// look at what they did.
func (r *Runner) passAndWait(ctx context.Context) {
	r.pass(ctx)
	r.jobs.Wait()
}

func TestAStackThatHangsHoldsUpNoOther(t *testing.T) {
	runner, cluster, docker, _ := newTestRunner(t)
	cluster.Put(resource.KindStack, "blog", stackspec.Spec{Services: map[string]stackspec.Service{"web": {Image: "x"}}})
	cluster.Place("blog")
	// the first compose up hangs until the test lets it go
	release, first := make(chan struct{}), make(chan struct{}, 1)
	first <- struct{}{}
	docker.DuringUp = func() {
		select {
		case <-first:
			<-release
		default:
		}
	}
	runner.pass(context.Background())
	deadline := time.Now().Add(5 * time.Second)
	for docker.Count("up "+shopProject)+docker.Count("up default_blog") < 2 {
		if time.Now().After(deadline) {
			close(release)
			t.Fatalf("one hanging compose up held up the other: %v", docker.Lines())
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(release)
	runner.jobs.Wait()
}

func TestAHostThatIsFencedStartsNoStack(t *testing.T) {
	for phase, starts := range map[state.HostPhase]bool{
		state.Failed: true, state.Fencing: true, state.Fenced: false, state.Recovering: false, state.Disabled: false,
	} {
		t.Run(string(phase), func(t *testing.T) {
			runner, cluster, docker, _ := newTestRunner(t)
			cluster.Read(func(current *state.State) { current.Hosts[0].Phase = phase })
			runner.passAndWait(context.Background())
			if started := docker.Count("up "+shopProject) > 0; started != starts {
				t.Errorf("a %s host started shop: %v", phase, started)
			}
		})
	}
}
