package stacks

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/webcodingcat/clowder/internal/agent/agenttest"
	"github.com/webcodingcat/clowder/internal/ingress"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/resource"
	"github.com/webcodingcat/clowder/internal/stackspec"
	"github.com/webcodingcat/clowder/internal/state"
)

func TestSecretLabelsAreKeyedAndUnambiguous(t *testing.T) {
	digests := map[string]string{"a": "bc", "ab": "c", "x": "1"}
	label := secretLabel([]byte("key"), digests, []string{"a", "x"})
	if label != secretLabel([]byte("key"), digests, []string{"x", "a"}) {
		t.Error("the order of the names changed the label")
	}
	for what, other := range map[string]string{
		"another key":                    secretLabel([]byte("other"), digests, []string{"a", "x"}),
		"another digest":                 secretLabel([]byte("key"), map[string]string{"a": "bc", "x": "2"}, []string{"a", "x"}),
		"the same bytes split elsewhere": secretLabel([]byte("key"), digests, []string{"ab", "x"}),
	} {
		if other == label {
			t.Errorf("%s gave the same label", what)
		}
	}
	if strings.Contains(label, "bc") || len(label) != 64 {
		t.Errorf("label %q", label)
	}
}

func TestAStackWhoseSecretsDoNotOpenDoesNotComeUp(t *testing.T) {
	tests := []struct {
		name string
		// lose makes the secret db-password fail to open
		lose func(runner *Runner, cluster *agenttest.Cluster)
	}{
		{"the data key does not open it", func(runner *Runner, _ *agenttest.Cluster) {
			runner.open = func(state.Sealed) ([]byte, error) { return nil, errors.New("no identity matched") }
		}},
		{"the state lost it", func(_ *Runner, cluster *agenttest.Cluster) {
			// a bug, not a command: the state refuses to delete a secret a
			// stack names
			cluster.Lock()
			delete(cluster.State.Secrets, "default/db-password")
			cluster.Unlock()
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner, cluster, docker, _ := newTestRunner(t)
			cluster.PutSecret("db-password", "first")
			cluster.Put(resource.KindStack, "shop", agenttest.UsingDB(stackspec.Service{Secrets: []string{"db-password"}}))
			open := runner.open
			test.lose(runner, cluster)
			runner.passAndWait(context.Background())
			if ups := docker.Count("up " + shopProject); ups != 0 {
				t.Errorf("%d compose ups without the secret", ups)
			}
			if _, err := os.Stat(runner.files.secretsDir(shopProject)); !os.IsNotExist(err) {
				t.Errorf("secrets were written: %v", err)
			}
			if stack := cluster.Stack("shop"); stack.Status != state.StackFailing || !strings.Contains(stack.UpError, "db-password") {
				t.Errorf("the stack is %s: %q", stack.Status, stack.UpError)
			}

			// once the secret opens, the next pass brings the stack up
			runner.open = open
			cluster.PutSecret("db-password", "first")
			runner.passAndWait(context.Background())
			if ups := docker.Count("up " + shopProject); ups != 1 {
				t.Errorf("%d compose ups once the secret opens, want 1", ups)
			}
		})
	}
}

func TestOnlyChangedSecretFilesAreReplaced(t *testing.T) {
	runner, cluster, _, _ := newTestRunner(t)
	cluster.PutSecret("db-password", "password")
	cluster.PutSecret("api-key", "first")
	cluster.Put(resource.KindStack, "shop", agenttest.UsingDB(stackspec.Service{Secrets: []string{"db-password", "api-key"}}))
	runner.passAndWait(context.Background())
	stat := func(name string) os.FileInfo {
		t.Helper()
		info, err := os.Stat(runner.files.secretFile(shopProject, name, "value"))
		if err != nil {
			t.Fatal(err)
		}
		return info
	}
	password, key := stat("db-password"), stat("api-key")

	cluster.PutSecret("api-key", "second")
	runner.passAndWait(context.Background())
	// a file is replaced by a rename: the same file means it was left alone
	if !os.SameFile(password, stat("db-password")) {
		t.Error("the unchanged secret file was written anew")
	}
	if os.SameFile(key, stat("api-key")) {
		t.Error("the changed secret file was not replaced")
	}
}

func TestTheSecretsGetTheirRightsBackAtEachPass(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("modes work otherwise on Windows")
	}
	runner, cluster, _, _ := newTestRunner(t)
	cluster.PutSecret("db-password", "password")
	cluster.Put(resource.KindStack, "shop", agenttest.UsingDB(stackspec.Service{Secrets: []string{"db-password"}}))
	dirs := []string{runner.files.secretsDir(shopProject), runner.files.secretFilesDir(shopProject)}
	secretDir := filepath.Join(runner.files.secretFilesDir(shopProject), "db-password")
	file := runner.files.secretFile(shopProject, "db-password", "value")
	// a directory that was there before the agent, with other rights
	if err := os.Mkdir(dirs[0], 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dirs[0], 0o777); err != nil {
		t.Fatal(err)
	}
	runner.passAndWait(context.Background())
	assertMode := func(path string, want os.FileMode) {
		t.Helper()
		if info, err := os.Stat(path); err != nil {
			t.Error(err)
		} else if info.Mode().Perm() != want {
			t.Errorf("%s has mode %v, want %v", path, info.Mode().Perm(), want)
		}
	}
	assertModes := func() {
		t.Helper()
		for _, dir := range dirs {
			assertMode(dir, 0o700)
		}
		// the container's user reads the secret's directory and its files
		assertMode(secretDir, 0o755)
		assertMode(file, 0o444)
	}
	assertModes()

	// the secrets are unchanged, so nothing is written, but the rights come back
	for _, path := range []string{dirs[0], dirs[1], secretDir, file} {
		if err := os.Chmod(path, 0o777); err != nil {
			t.Fatal(err)
		}
	}
	runner.passAndWait(context.Background())
	assertModes()
}

func TestASecretFileTheStackNoLongerAsksForGoes(t *testing.T) {
	runner, cluster, docker, _ := newTestRunner(t)
	cluster.PutSecret("db-password", "password")
	cluster.PutSecret("api-key", "key")
	shop := agenttest.UsingDB(stackspec.Service{Secrets: []string{"db-password", "api-key"}})
	// a second service with the same secret shares its file
	shop.Services["worker"] = stackspec.Service{Image: "worker", Secrets: []string{"api-key"}}
	cluster.Put(resource.KindStack, "shop", shop)
	runner.passAndWait(context.Background())
	files, err := os.ReadDir(runner.files.secretFilesDir(shopProject))
	if err != nil || len(files) != 2 {
		t.Fatalf("secret files %v, %v; want 2", files, err)
	}

	cluster.Put(resource.KindStack, "shop", agenttest.UsingDB(stackspec.Service{Secrets: []string{"db-password"}}))
	docker.FailedUps = 1
	runner.passAndWait(context.Background())
	// until compose up worked, the old containers may still read it
	if _, err := os.Stat(runner.files.secretFile(shopProject, "api-key", "value")); err != nil {
		t.Errorf("the secret file went before compose up worked: %v", err)
	}
	runner.passAndWait(context.Background())
	if _, err := os.Stat(filepath.Join(runner.files.secretFilesDir(shopProject), "api-key")); !os.IsNotExist(err) {
		t.Errorf("the secret no service asks for stayed: %v", err)
	}
	if _, err := os.Stat(runner.files.secretFile(shopProject, "db-password", "value")); err != nil {
		t.Errorf("the secret file the stack still asks for: %v", err)
	}
}

func TestTheSecretsOfAStackThatLeftGoOnlyOnceItStopped(t *testing.T) {
	runner, cluster, docker, _ := newTestRunner(t)
	cluster.PutSecret("db-password", "password")
	cluster.Put(resource.KindStack, "shop", agenttest.UsingDB(stackspec.Service{Secrets: []string{"db-password"}}))
	runner.passAndWait(context.Background())
	// what a crash left, and the secrets of clowder's own Traefik
	for _, project := range []string{"default_crashed", ingress.Project} {
		if err := os.MkdirAll(filepath.Join(runner.files.secretsDir(project), "files"), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	cluster.Apply(state.KindSetStackStatus, state.LeaderActor, state.SetStackStatus{
		ID: "default/shop", Status: state.StackPending, PlannedAt: cluster.Index(),
	})
	docker.FailedDowns = 1
	runner.passAndWait(context.Background())
	exists := func(project string) bool {
		_, err := os.Stat(runner.files.secretsDir(project))
		return err == nil
	}
	if !exists(shopProject) {
		t.Error("the secrets went while the stack's containers may still run")
	}
	if exists("default_crashed") {
		t.Error("the secrets a crash left stayed")
	}
	runner.passAndWait(context.Background())
	if exists(shopProject) {
		t.Error("the secrets of the stopped stack stayed")
	}
	if !exists(ingress.Project) {
		t.Error("the secrets of clowder's own Traefik went")
	}
}

func TestEachKeyOfASecretIsAFileAndStaleKeysGo(t *testing.T) {
	runner, cluster, _, _ := newTestRunner(t)
	cluster.PutSecretIn(names.Default, "db", map[string]string{"user": "shop", "password": "first"})
	cluster.PutSecret("tls", "cert")
	cluster.Put(resource.KindStack, "shop", agenttest.UsingDB(stackspec.Service{Secrets: []string{"db", "tls"}}))
	runner.passAndWait(context.Background())
	// at <files>/<secret>/<key>, which the Compose file mounts at
	// /run/secrets/<secret>/<key>
	assertContent := func(name, key, want string) {
		t.Helper()
		path := filepath.Join(runner.files.secretFilesDir(shopProject), name, key)
		if content, err := os.ReadFile(path); err != nil || string(content) != want {
			t.Errorf("%s/%s: %q, %v; want %q", name, key, content, err, want)
		}
	}
	assertContent("db", "user", "shop")
	assertContent("db", "password", "first")
	assertContent("tls", "value", "cert")

	// db loses its key user, and the stack no longer asks for tls
	cluster.PutSecretIn(names.Default, "db", map[string]string{"password": "second"})
	cluster.Put(resource.KindStack, "shop", agenttest.UsingDB(stackspec.Service{Secrets: []string{"db"}}))
	runner.passAndWait(context.Background())
	assertContent("db", "password", "second")
	if _, err := os.Stat(runner.files.secretFile(shopProject, "db", "user")); !os.IsNotExist(err) {
		t.Errorf("the key the secret lost stayed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(runner.files.secretFilesDir(shopProject), "tls")); !os.IsNotExist(err) {
		t.Errorf("the secret the stack no longer asks for stayed: %v", err)
	}
}

func TestASecretVariableTakesTheValueOfItsKey(t *testing.T) {
	runner, cluster, _, _ := newTestRunner(t)
	cluster.PutSecretIn(names.Default, "db", map[string]string{"user": "shop", "password": "pw"})
	cluster.Put(resource.KindStack, "shop", agenttest.UsingDB(stackspec.Service{SecretEnv: map[string]names.SecretKeyRef{
		"DB_USER": {Name: "db", Key: "user"}, "DB_PASSWORD": {Name: "db", Key: "password"},
	}}))
	runner.passAndWait(context.Background())
	content, err := os.ReadFile(runner.files.secretEnvFile(shopProject))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"DB_USER: shop", "DB_PASSWORD: pw"} {
		if !strings.Contains(string(content), want) {
			t.Errorf("no %q in the secret variables:\n%s", want, content)
		}
	}
	if stack := cluster.Stack("shop"); stack.Status != state.StackRunning {
		t.Errorf("the stack is %s: %q", stack.Status, stack.UpError)
	}
}
