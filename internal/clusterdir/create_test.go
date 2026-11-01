package clusterdir

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/webcodingcat/clowder/internal/clusterspec"
)

const testArmor = "-----BEGIN AGE ENCRYPTED FILE-----\nam9pbg==\n-----END AGE ENCRYPTED FILE-----\n"

func TestTheBootstrapsClusterYAMLLoadsAndNamesWhatIsMissing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "prod")
	if err := Create(NewDirectory{Path: dir, Name: "prod", Seed: "203.0.113.10", JoinKey: testArmor}); err != nil {
		t.Fatal(err)
	}
	directory, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	config := directory.Config
	if config.Name != "prod" || !slices.Equal(config.Seeds, []string{"203.0.113.10"}) ||
		!slices.Equal(config.JoinKeys, []clusterspec.SecretRef{"join_keys[0]"}) {
		t.Errorf("config %+v", config)
	}
	if directory.Encrypted["join_keys[0]"] != testArmor {
		t.Errorf("the join key %q", directory.Encrypted["join_keys[0]"])
	}
	problems := clusterspec.Check(config).Error()
	for _, missing := range []string{"hoster.token", "floating_ip", "storage_classes"} {
		if !strings.Contains(problems, missing) {
			t.Errorf("Check does not name %s:\n%s", missing, problems)
		}
	}
	if err := Create(NewDirectory{Path: dir, Name: "prod", JoinKey: testArmor}); err == nil {
		t.Error("a second Create wrote over cluster.yaml")
	}
}
