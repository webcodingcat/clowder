package state

import (
	"maps"
	"testing"

	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/resource"
	"github.com/webcodingcat/clowder/internal/stackspec"
)

func TestAHostMayReadOnlyTheSecretsOfTheStacksItRuns(t *testing.T) {
	c := newCluster(t)
	uses := shopSpec()
	uses.Services["app"] = stackspec.Service{
		Image: "x", SecretEnv: map[string]names.SecretKeyRef{"DB_PASSWORD": {Name: "db", Key: "password"}},
	}
	c.mustApply(KindChangeResources, cliActor, batch(
		registryLogin("ghcr", resource.RegistryLoginKeys...),
		pullingWith("ghcr"),
		registryLogin("db", "password"),
		registryLogin("other", "password"),
		putChange(resource.KindStack, names.Default, "shop", uses),
	))
	if readable := c.state.SecretsOfHost(0); len(readable) != 0 {
		t.Errorf("before the stack is placed, host 0 may read %v", readable)
	}
	host := HostID(0)
	c.mustApply(KindSetStackStatus, LeaderActor, SetStackStatus{PlannedAt: c.index, ID: "default/shop", Status: StackStarting, Host: &host})
	want := map[string]bool{"default/db": true, "default/ghcr": true}
	if readable := c.state.SecretsOfHost(0); !maps.Equal(readable, want) {
		t.Errorf("host 0 may read %v, want %v", readable, want)
	}
	if readable := c.state.SecretsOfHost(1); len(readable) != 0 {
		t.Errorf("a host without the stack may read %v", readable)
	}
}
