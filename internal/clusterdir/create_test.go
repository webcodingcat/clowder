package clusterdir

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/webcodingcat/clowder/internal/clusterspec"
)

func TestTheBootstrapsClusterYAMLLoadsAndNamesWhatIsMissing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "prod")
	if err := Create(NewDirectory{Path: dir, Name: "prod", Endpoint: "203.0.113.10"}); err != nil {
		t.Fatal(err)
	}
	directory, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	config := directory.Config
	if config.Name != "prod" || !slices.Equal(config.Endpoints, []string{"203.0.113.10"}) || len(directory.Encrypted) != 0 {
		t.Errorf("config %+v, values %v", config, directory.Encrypted)
	}
	problems := clusterspec.Check(config).Error()
	for _, missing := range []string{"hoster.token", "floating_ip", "storage_classes"} {
		if !strings.Contains(problems, missing) {
			t.Errorf("Check does not name %s:\n%s", missing, problems)
		}
	}
	if err := Create(NewDirectory{Path: dir, Name: "prod"}); err == nil {
		t.Error("a second Create wrote over cluster.yaml")
	}
}
