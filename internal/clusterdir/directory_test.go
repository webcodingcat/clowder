package clusterdir

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
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
	if !reflect.DeepEqual(directory.Config.Priorities, DefaultPriorities()) {
		t.Errorf("priorities: %+v", directory.Config.Priorities)
	}
}

func TestPriorityDefaultsAreCheckedAndFilledIn(t *testing.T) {
	tests := map[string]struct {
		priorities  Priorities
		wantProblem string
		wantDefault string
	}{
		"nothing":                     {Priorities{}, "", "normal"},
		"default of the defaults":     {Priorities{Default: "high"}, "", "high"},
		"misspelled default":          {Priorities{Default: "hihg"}, `"hihg" is not one of`, ""},
		"own categories, no default":  {Priorities{Categories: map[string]int{"top": 900}}, "default is required", ""},
		"own categories with default": {Priorities{Default: "top", Categories: map[string]int{"top": 900}}, "", "top"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			problems := checkPriorities(test.priorities)
			if test.wantProblem == "" {
				if len(problems) > 0 {
					t.Fatal(problems)
				}
				if got := test.priorities.withDefaults().Default; got != test.wantDefault {
					t.Errorf("default %q, want %q", got, test.wantDefault)
				}
				return
			}
			if len(problems) == 0 || !strings.Contains(problems.Error(), test.wantProblem) {
				t.Errorf("want a problem with %q, got %v", test.wantProblem, problems)
			}
		})
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
	if _, err := directory.Credentials("../secrets"); err == nil || !strings.Contains(err.Error(), "user name") {
		t.Errorf("want an error about the user name, got %v", err)
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
		Alerts: &Alerts{
			Channels: []AlertChannel{
				{Type: "discord"},
				{Type: "pager"},
				{Type: "webhook", MinSeverity: "loud"},
				{Type: "email", SMTP: &SMTP{}, From: "clowder@example.com", To: []string{"ops@example.com"}},
			},
			Heartbeat: &Heartbeat{Interval: "often"},
		},
		Metrics: Metrics{Listen: "everywhere"},
	}
	want := []string{
		"name is required", `"10.0.0.1:99999"`, "hoster is required", "hoster_token",
		`network "both"`, "not an IPv4 address", `pool "nopool"`, `reserve "95%"`,
		`falls back to "slow"`, "storage class empty", "resync_rate", "top: 2000",
		`default "medium"`, "reservations.memory", "reservations.cpus", `"borg"`,
		"webhook: {secret", "type is not discord", `min_severity "loud"`, "url: {secret",
		"heartbeat.url", `interval "often"`, `listen "everywhere"`, "backup.target.url", "backup.password",
		"smtp needs host and port",
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
	missing := Check(Config{}).Error()
	for _, key := range schema.Required {
		if !strings.Contains(missing, key) {
			t.Errorf("the schema requires %s, but Check does not report it missing:\n%s", key, missing)
		}
	}
}

func TestProblemsAreAnError(t *testing.T) {
	var err error = Problems{"a"}
	var problems Problems
	if !errors.As(err, &problems) {
		t.Error("Problems is not usable with errors.As")
	}
}

func TestUserComesFromTheFlagTheEnvironmentOrTheOnlyCredentials(t *testing.T) {
	directory, err := Load(filepath.Join("testdata", "prod"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(UserEnvVar, "")
	if user, err := directory.User(""); err != nil || user != "alice" {
		t.Errorf("only credentials: %q, %v", user, err)
	}
	t.Setenv(UserEnvVar, "bob")
	if user, _ := directory.User(""); user != "bob" {
		t.Errorf("environment: %q", user)
	}
	if user, _ := directory.User("carol"); user != "carol" {
		t.Errorf("flag: %q", user)
	}
}
