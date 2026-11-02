package agent

import (
	"context"
	"maps"
	"testing"

	"github.com/webcodingcat/clowder/internal/clusterspec"
	"github.com/webcodingcat/clowder/internal/ingress"
	"github.com/webcodingcat/clowder/internal/state"
	"github.com/webcodingcat/clowder/internal/supervisor"
)

func TestTheRestartRulesComeFromTheStacksOfThisHost(t *testing.T) {
	_, cluster, _, _ := newTestRunner(t)
	rules := restartRules{cluster, 0}
	tests := []struct {
		name, project, service string
		want                   supervisor.Rule
		known                  bool
	}{
		{"clowder's Traefik", ingress.Project, "traefik", traefikRule, true},
		{"a service of a stack here", shopProject, "app", supervisor.Rule{Restart: "always", RestartUnhealthy: true}, true},
		{"a service the stack lacks", shopProject, "cache", supervisor.Rule{}, false},
		{"a stack the state lacks", "default_blog", "app", supervisor.Rule{}, false},
		{"no project of a stack", "not a project", "app", supervisor.Rule{}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if rule, known := rules.of(test.project, test.service); rule != test.want || known != test.known {
				t.Errorf("%+v, %v; want %+v, %v", rule, known, test.want, test.known)
			}
		})
	}

	// a stack that moved to another host is not this host's to restart
	cluster.mu.Lock()
	other := state.HostID(1)
	cluster.state.Stacks["default/shop"].Host = &other
	cluster.mu.Unlock()
	if rule, known := rules.of(shopProject, "app"); known {
		t.Errorf("the rule of a stack on another host: %+v", rule)
	}
}

func TestDockerLogsInOnceForEachRegistryLogin(t *testing.T) {
	runner, cluster, docker, _ := newTestRunner(t)
	login := func(password string) {
		t.Helper()
		sealed, err := cluster.keys.Seal([]byte(password))
		if err != nil {
			t.Fatal(err)
		}
		var applied state.ApplyConfig
		cluster.Read(func(current *state.State) {
			applied = state.ApplyConfig{Spec: current.Config.Spec, Secrets: maps.Clone(current.ClusterSecrets)}
		})
		name := `registries["ghcr.io"].password`
		applied.Spec.Registries = map[string]clusterspec.Registry{"ghcr.io": {Username: "robot", Password: clusterspec.SecretRef(name)}}
		applied.Secrets[name] = state.SealedSecret{Value: sealed, Digest: name}
		cluster.apply(state.KindApplyConfig, state.UserActor("alice"), applied)
	}
	login("first")
	runner.pass(context.Background())
	login("first") // sealed anew, the same login
	runner.pass(context.Background())
	login("second")
	runner.pass(context.Background())
	if docker.count("login ghcr.io robot first") != 1 || docker.count("login ghcr.io robot second") != 1 {
		t.Errorf("docker %q", docker.lines)
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
