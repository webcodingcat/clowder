package stacks

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/webcodingcat/clowder/internal/agent/agenttest"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/resource"
	"github.com/webcodingcat/clowder/internal/state"
)

// pullWith gives the namespace default the image pull secrets.
func pullWith(cluster *agenttest.Cluster, secrets ...string) {
	var spec resource.NamespaceSpec
	for _, name := range secrets {
		spec.ImagePullSecrets = append(spec.ImagePullSecrets, resource.LocalObjectReference{Name: name})
	}
	cluster.PutIn("", resource.KindNamespace, names.Default, spec)
}

// readDockerConfig reads the Docker config of the namespace default.
func readDockerConfig(t *testing.T, runner *Runner) dockerConfig {
	t.Helper()
	path := filepath.Join(runner.files.registryConfigDir(names.Default), "config.json")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var config dockerConfig
	if err := json.Unmarshal(content, &config); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o600) {
		t.Errorf("the Docker config's mode: %v, %v", info, err)
	}
	return config
}

func TestAStackPullsWithTheDockerConfigOfItsNamespacesPullSecrets(t *testing.T) {
	runner, cluster, docker, _ := newTestRunner(t)
	cluster.PutSecretIn(names.Default, "ghcr", map[string]string{"server": "ghcr.io", "username": "robot", "password": "first"})
	cluster.PutSecretIn(names.Default, "own", map[string]string{
		"server": "registry.example.com", "username": "ci", "password": "pass:word",
	})
	pullWith(cluster, "ghcr", "own")
	runner.passAndWait(context.Background())
	if docker.UpConfig != runner.files.registryConfigDir(names.Default) {
		t.Errorf("compose up with the Docker config %q", docker.UpConfig)
	}
	if !strings.HasPrefix(docker.UpConfig, runner.files.SecretsDir) {
		t.Errorf("the Docker config %q is not on the secrets' tmpfs", docker.UpConfig)
	}
	want := map[string]string{"ghcr.io": "robot:first", "registry.example.com": "ci:pass:word"}
	config := readDockerConfig(t, runner)
	if len(config.Auths) != len(want) {
		t.Errorf("auths %v, want %v", config.Auths, want)
	}
	for server, login := range want {
		if got := config.Auths[server].Auth; got != base64.StdEncoding.EncodeToString([]byte(login)) {
			t.Errorf("the auth of %s: %q, want base64 of %q", server, got, login)
		}
	}

	// a new password: the config follows
	cluster.PutSecretIn(names.Default, "ghcr", map[string]string{"server": "ghcr.io", "username": "robot", "password": "second"})
	runner.passAndWait(context.Background())
	if got := readDockerConfig(t, runner).Auths["ghcr.io"].Auth; got != base64.StdEncoding.EncodeToString([]byte("robot:second")) {
		t.Errorf("after the new password: %q", got)
	}
}

func TestANamespaceWithoutPullSecretsGetsAnEmptyDockerConfig(t *testing.T) {
	runner, _, docker, _ := newTestRunner(t)
	runner.passAndWait(context.Background())
	// an empty config, not none: a pull never takes root's own logins
	if config := readDockerConfig(t, runner); config.Auths == nil || len(config.Auths) != 0 {
		t.Errorf("auths %v", config.Auths)
	}
	if docker.UpConfig != runner.files.registryConfigDir(names.Default) {
		t.Errorf("compose up with the Docker config %q", docker.UpConfig)
	}
}

func TestAStackWithoutItsPullSecretComesUpOrFailsWithTheReason(t *testing.T) {
	for _, test := range []struct {
		name string
		// put makes the image pull secret ghcr, or not
		put  func(cluster *agenttest.Cluster)
		want string
	}{
		{"no such secret", func(*agenttest.Cluster) {}, "image pull secret ghcr"},
		{"no password", func(cluster *agenttest.Cluster) {
			cluster.PutSecretIn(names.Default, "ghcr", map[string]string{"server": "ghcr.io", "username": "robot"})
		}, "has no key password"},
	} {
		t.Run(test.name, func(t *testing.T) {
			// as the kubelet: an image that needs no login pulls without it
			runner, cluster, _, _ := newTestRunner(t)
			test.put(cluster)
			pullWith(cluster, "ghcr")
			runner.passAndWait(context.Background())
			if stack := cluster.Stack("shop"); stack.Status != state.StackRunning {
				t.Errorf("a stack of public images is %s: %q", stack.Status, stack.UpError)
			}

			// a pull that fails says which login was left out
			runner, cluster, docker, _ := newTestRunner(t)
			test.put(cluster)
			pullWith(cluster, "ghcr")
			docker.FailedUps = 1
			runner.passAndWait(context.Background())
			if stack := cluster.Stack("shop"); stack.Status != state.StackFailing || !strings.Contains(stack.UpError, test.want) {
				t.Errorf("the stack is %s: %q, want %q in it", stack.Status, stack.UpError, test.want)
			}
		})
	}
}
