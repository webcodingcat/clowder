package stacks

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/webcodingcat/clowder/internal/agent/agenttest"
	"github.com/webcodingcat/clowder/internal/ingress"
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
			runner.pass(context.Background())
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
			runner.pass(context.Background())
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
	runner.pass(context.Background())
	stat := func(name string) os.FileInfo {
		t.Helper()
		info, err := os.Stat(runner.files.secretFile(shopProject, name))
		if err != nil {
			t.Fatal(err)
		}
		return info
	}
	password, key := stat("db-password"), stat("api-key")

	cluster.PutSecret("api-key", "second")
	runner.pass(context.Background())
	// a file is replaced by a rename: the same file means it was left alone
	if !os.SameFile(password, stat("db-password")) {
		t.Error("the unchanged secret file was written anew")
	}
	if os.SameFile(key, stat("api-key")) {
		t.Error("the changed secret file was not replaced")
	}
}

func TestASecretFileTheStackNoLongerAsksForGoes(t *testing.T) {
	runner, cluster, docker, _ := newTestRunner(t)
	cluster.PutSecret("db-password", "password")
	cluster.PutSecret("api-key", "key")
	shop := agenttest.UsingDB(stackspec.Service{Secrets: []string{"db-password", "api-key"}})
	// a second service with the same secret shares its file
	shop.Services["worker"] = stackspec.Service{Image: "worker", Secrets: []string{"api-key"}}
	cluster.Put(resource.KindStack, "shop", shop)
	runner.pass(context.Background())
	files, err := os.ReadDir(runner.files.secretFilesDir(shopProject))
	if err != nil || len(files) != 2 {
		t.Fatalf("secret files %v, %v; want 2", files, err)
	}

	cluster.Put(resource.KindStack, "shop", agenttest.UsingDB(stackspec.Service{Secrets: []string{"db-password"}}))
	docker.FailedUps = 1
	runner.pass(context.Background())
	// until compose up worked, the old containers may still read it
	if _, err := os.Stat(runner.files.secretFile(shopProject, "api-key")); err != nil {
		t.Errorf("the secret file went before compose up worked: %v", err)
	}
	runner.pass(context.Background())
	if _, err := os.Stat(runner.files.secretFile(shopProject, "api-key")); !os.IsNotExist(err) {
		t.Errorf("the secret file no service asks for stayed: %v", err)
	}
	if _, err := os.Stat(runner.files.secretFile(shopProject, "db-password")); err != nil {
		t.Errorf("the secret file the stack still asks for: %v", err)
	}
}

func TestTheSecretsOfAStackThatLeftGoOnlyOnceItStopped(t *testing.T) {
	runner, cluster, docker, _ := newTestRunner(t)
	cluster.PutSecret("db-password", "password")
	cluster.Put(resource.KindStack, "shop", agenttest.UsingDB(stackspec.Service{Secrets: []string{"db-password"}}))
	runner.pass(context.Background())
	// what a crash left, and the secrets of clowder's own Traefik
	for _, project := range []string{"default_crashed", ingress.Project} {
		if err := os.MkdirAll(filepath.Join(runner.files.secretsDir(project), "files"), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	cluster.Apply(state.KindSetStackStatus, state.LeaderActor, state.SetStackStatus{ID: "default/shop", Status: state.StackPending})
	docker.FailedDowns = 1
	runner.pass(context.Background())
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
	runner.pass(context.Background())
	if exists(shopProject) {
		t.Error("the secrets of the stopped stack stayed")
	}
	if !exists(ingress.Project) {
		t.Error("the secrets of clowder's own Traefik went")
	}
}
