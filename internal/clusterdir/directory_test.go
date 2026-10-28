package clusterdir

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// useTestKey points SOPS at the test key in testdata.
func useTestKey(t *testing.T) {
	t.Helper()
	key, err := filepath.Abs(filepath.Join("testdata", "test-age-key.txt"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SOPS_AGE_KEY_FILE", key)
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
	credentials, err := directory.Credentials("alice")
	if err != nil {
		t.Fatal(err)
	}
	if credentials.Certificate != "TEST-CERT-PEM" {
		t.Errorf("credentials: %+v", credentials)
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

func TestCheckReportsEveryProblem(t *testing.T) {
	config := Config{
		Seeds:   []string{"10.0.0.1:99999"},
		Network: "both",
		Ingress: Ingress{FloatingIP: "2001:db8::1"},
		StorageClasses: map[string]StorageClass{
			"fast":  {Pool: "nopool", Reserve: "95%"},
			"any":   {Fallback: []string{"slow"}},
			"empty": {},
		},
		Replication: Replication{ResyncRate: "fast"},
		Priorities:  Priorities{Default: "medium", Categories: map[string]int{"top": 2000}},
		Defaults:    Defaults{Reservations: Resources{Memory: "lots", CPUs: "-1"}},
		Backup:      &Backup{Target: BackupTarget{Type: "borg"}},
		Alerts:      &Alerts{Channels: []AlertChannel{{Type: "discord"}, {Type: "pager"}}},
	}
	want := []string{
		"name is required", `"10.0.0.1:99999"`, "hoster is required", "hoster_token",
		`network "both"`, "not an IPv4 address", `pool "nopool"`, `reserve "95%"`,
		`falls back to "slow"`, "storage class empty", "resync_rate", "top: 2000",
		`default "medium"`, "reservations.memory", "reservations.cpus", `"borg"`,
		"webhook: {secret", "type is not discord",
	}
	problems := Check(config)
	text := problems.Error()
	for _, fragment := range want {
		if !strings.Contains(text, fragment) {
			t.Errorf("missing %q in:\n%s", fragment, text)
		}
	}
	if len(problems) != len(want) {
		t.Errorf("want %d problems, got %d:\n%s", len(want), len(problems), text)
	}
}

func TestEverySecretReferenceIsFound(t *testing.T) {
	directory, err := Load(filepath.Join("testdata", "prod"))
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(SecretNames(directory.Config), ",")
	if got != "discord-webhook,hcloud-token,restic-password" {
		t.Errorf("got %s", got)
	}
}

func TestFindPrefersTheFlagThenTheEnvironment(t *testing.T) {
	t.Setenv(EnvVar, "/from/env")
	if dir, _ := Find("/from/flag"); dir != "/from/flag" {
		t.Errorf("flag: %s", dir)
	}
	if dir, _ := Find(""); dir != "/from/env" {
		t.Errorf("env: %s", dir)
	}
	t.Setenv(EnvVar, "")
	t.Chdir(t.TempDir())
	if _, err := Find(""); err == nil {
		t.Error("no error without a cluster directory")
	}
}

func TestTheSchemaDescribesCluterYAML(t *testing.T) {
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
	if !strings.Contains(strings.Join(schema.Required, ","), "seeds") {
		t.Errorf("seeds not required: %v", schema.Required)
	}
}

func TestProblemsAreAnError(t *testing.T) {
	var err error = Problems{"a"}
	var problems Problems
	if !errors.As(err, &problems) {
		t.Error("Problems is not usable with errors.As")
	}
}
