package clusterdir

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/webcodingcat/clowder/internal/clusterspec"
)

// withConfig writes the example's cluster.yaml, changed, into a directory
// of its own.
func withConfig(t *testing.T, change func(content string) string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join("testdata", "prod", ConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	changed := change(string(content))
	if changed == string(content) {
		t.Fatal("the change found nothing to change in the example")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ConfigFile), []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestTheExampleDirectoryLoadsWithItsValuesApart(t *testing.T) {
	directory, err := Load(filepath.Join("testdata", "prod"))
	if err != nil {
		t.Fatal(err)
	}
	config := directory.Config
	if config.Name != "prod" || config.Ingress.FloatingIP != "198.51.100.7" || config.Hoster.Token != "hoster.token" {
		t.Errorf("config: %+v", config)
	}
	if config.StorageClasses["any"].Fallback[0] != "fast" || config.Priorities.Categories["high"] != 700 {
		t.Errorf("classes or priorities: %+v %+v", config.StorageClasses, config.Priorities)
	}
	want := []string{
		"alerts.channels[0].webhook", "backup.password", "hoster.token", "join_keys[0]",
		`registries["ghcr.io"].password`,
	}
	if got := slices.Sorted(maps.Keys(directory.Encrypted)); !slices.Equal(got, want) {
		t.Errorf("values %q", got)
	}
	if !strings.Contains(directory.Encrypted["join_keys[0]"], "am9pbg==") {
		t.Errorf("the join key's armor: %q", directory.Encrypted["join_keys[0]"])
	}
	if problems := clusterspec.Check(config); len(problems) > 0 {
		t.Errorf("the example does not pass Check: %v", problems)
	}
}

func TestASecretStandsOnlyWhereOneMay(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(content string) string
		want   string
	}{
		{"in clear", func(content string) string {
			return strings.Replace(content, "  password: !secret |\n    -----BEGIN AGE ENCRYPTED FILE-----\n"+
				"    YmFja3Vw\n    -----END AGE ENCRYPTED FILE-----\n", "  password: backup.password\n", 1)
		}, "backup.password: write the value encrypted"},
		{"elsewhere", func(content string) string {
			return strings.Replace(content, "username: robot", "username: !secret |\n      abc", 1)
		}, `registries["ghcr.io"].username: !secret is not allowed here`},
		{"two documents", func(content string) string { return content + "---\nname: other\n" }, "2 YAML documents"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Load(withConfig(t, test.change)); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Errorf("got %v, want %q", err, test.want)
			}
		})
	}
}

func TestMisspeltKeysAreRefused(t *testing.T) {
	dir := withConfig(t, func(content string) string { return strings.Replace(content, "seeds:", "seed:", 1) })
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "seed") {
		t.Errorf("want an error about seed, got %v", err)
	}
}

func TestFindPrefersTheFlagThenTheEnvironment(t *testing.T) {
	t.Setenv(EnvVar, "/from/env")
	if dir, err := Find("/from/flag"); err != nil || dir != "/from/flag" {
		t.Errorf("flag: %s, %v", dir, err)
	}
	if dir, err := Find(""); err != nil || dir != "/from/env" {
		t.Errorf("env: %s, %v", dir, err)
	}
	t.Setenv(EnvVar, "")
	t.Chdir(t.TempDir())
	if _, err := Find(""); err == nil {
		t.Error("no error without a cluster directory")
	}
}

func TestTheSchemaDescribesClusterYAML(t *testing.T) {
	encoded, err := Schema()
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties map[string]any `json:"properties"`
		Required   []string       `json:"required"`
	}
	if err := json.Unmarshal(encoded, &schema); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"name", "seeds", "storage_classes", "priorities", "alerts", "join_keys", "registries"} {
		if _, ok := schema.Properties[key]; !ok {
			t.Errorf("schema lacks %s", key)
		}
	}
	// what the schema requires, Check reports as missing in an empty file
	missing := clusterspec.Check(clusterspec.Config{}).Error()
	for _, key := range schema.Required {
		if !strings.Contains(missing, key) {
			t.Errorf("the schema requires %s, but Check does not report it missing:\n%s", key, missing)
		}
	}
}
