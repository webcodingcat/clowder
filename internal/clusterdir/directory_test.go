package clusterdir

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/webcodingcat/clowder/internal/clusterspec"
)

// useTestKey points SOPS at the test key in testdata.
func useTestKey(t *testing.T) {
	t.Helper()
	key, err := filepath.Abs(filepath.Join("testdata", "test-age-key.txt"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SOPS_AGE_KEY_FILE", key)
	// no SSH key of the developer's home joins in
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("SOPS_AGE_KEY", "")
}

func TestTheExampleDirectoryLoads(t *testing.T) {
	directory, err := Load(filepath.Join("testdata", "prod"))
	if err != nil {
		t.Fatal(err)
	}
	config := directory.Config
	if config.Name != "prod" || config.Ingress.FloatingIP != "198.51.100.7" {
		t.Errorf("config: %+v", config)
	}
	if config.StorageClasses["any"].Fallback[0] != "fast" || config.Priorities.Categories["high"] != 700 {
		t.Errorf("classes or priorities: %+v %+v", config.StorageClasses, config.Priorities)
	}
}

func TestLoadFillsInTheDefaultPriorities(t *testing.T) {
	content, err := os.ReadFile(filepath.Join("testdata", "prod", ConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	block := "priorities:\n  default: normal\n  categories: {critical: 900, high: 700, normal: 500, low: 200}\n"
	if !strings.Contains(string(content), block) {
		t.Fatal("the fixture's priorities block changed")
	}
	dir := t.TempDir()
	without := strings.Replace(string(content), block, "", 1)
	if err := os.WriteFile(filepath.Join(dir, ConfigFile), []byte(without), 0o600); err != nil {
		t.Fatal(err)
	}
	directory, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(directory.Config.Priorities, clusterspec.DefaultPriorities()) {
		t.Errorf("priorities: %+v", directory.Config.Priorities)
	}
}

func TestSecretsAreDecryptedWithTheUsersAgeKey(t *testing.T) {
	useTestKey(t)
	directory, err := Load(filepath.Join("testdata", "prod"))
	if err != nil {
		t.Fatal(err)
	}
	secrets, err := directory.Secrets()
	if err != nil {
		t.Fatal(err)
	}
	if secrets.Values["hcloud-token"] != "test-hcloud-token" || secrets.JoinKeys[0] != "test-join-key" {
		t.Errorf("secrets: %+v", secrets)
	}
	if secrets.Registries["ghcr.io"].Username != "robot" {
		t.Errorf("registries: %+v", secrets.Registries)
	}
}

func TestDecryptingWithoutTheKeyExplainsWhy(t *testing.T) {
	t.Setenv("SOPS_AGE_KEY_FILE", filepath.Join(t.TempDir(), "missing.txt"))
	t.Setenv("SOPS_AGE_KEY", "")
	directory, err := Load(filepath.Join("testdata", "prod"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := directory.Secrets(); err == nil || !strings.Contains(err.Error(), "age key") {
		t.Errorf("want a hint about the age key, got %v", err)
	}
}

func TestMisspeltKeysAreRefused(t *testing.T) {
	dir := t.TempDir()
	content, err := os.ReadFile(filepath.Join("testdata", "prod", ConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	misspelled := strings.Replace(string(content), "seeds:", "seed:", 1)
	if err := os.WriteFile(filepath.Join(dir, ConfigFile), []byte(misspelled), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "seed") {
		t.Errorf("want an error about seed, got %v", err)
	}
}

func TestTheFlatHosterFieldsAreRefused(t *testing.T) {
	dir := t.TempDir()
	content, err := os.ReadFile(filepath.Join("testdata", "prod", ConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	flat := strings.Replace(string(content), "hoster:\n  type: hetzner\n  token: {secret: hcloud-token}\n",
		"hoster: hetzner\nhoster_token: {secret: hcloud-token}\n", 1)
	if flat == string(content) {
		t.Fatal("the example has no hoster section to replace")
	}
	if err := os.WriteFile(filepath.Join(dir, ConfigFile), []byte(flat), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "hoster") {
		t.Errorf("want an error about hoster, got %v", err)
	}
}

func TestEverySecretReferenceIsFound(t *testing.T) {
	directory, err := Load(filepath.Join("testdata", "prod"))
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(clusterspec.SecretNames(directory.Config), ",")
	if got != "discord-webhook,hcloud-token,restic-password" {
		t.Errorf("got %s", got)
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
	for _, key := range []string{"name", "seeds", "storage_classes", "priorities", "alerts"} {
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
