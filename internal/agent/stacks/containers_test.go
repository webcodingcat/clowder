package stacks

import (
	"context"
	"maps"
	"testing"

	"github.com/webcodingcat/clowder/internal/clusterspec"
	"github.com/webcodingcat/clowder/internal/state"
)

func TestDockerLogsInOnceForEachRegistryLogin(t *testing.T) {
	runner, cluster, docker, _ := newTestRunner(t)
	login := func(password string) {
		t.Helper()
		sealed, err := cluster.Keys.Seal([]byte(password))
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
		cluster.Apply(state.KindApplyConfig, state.CLIActor("admin"), applied)
	}
	login("first")
	runner.passAndWait(context.Background())
	login("first") // sealed anew, the same login
	runner.passAndWait(context.Background())
	login("second")
	runner.passAndWait(context.Background())
	if docker.Count("login ghcr.io robot first") != 1 || docker.Count("login ghcr.io robot second") != 1 {
		t.Errorf("docker %q", docker.Lines())
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
