package clientconfig

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func testContext(name string) Context {
	return Context{
		Name: name, Endpoints: []string{"203.0.113.10", "203.0.113.11:7443"},
		CA: "ca", Certificate: "certificate", Key: "key",
	}
}

func TestThePathIsTheFlagThenTheEnvironmentThenTheHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv(EnvVar, "/from/env")
	if path, err := Path("/from/flag"); err != nil || path != "/from/flag" {
		t.Errorf("flag: %s, %v", path, err)
	}
	if path, err := Path(""); err != nil || path != "/from/env" {
		t.Errorf("env: %s, %v", path, err)
	}
	t.Setenv(EnvVar, "")
	if path, err := Path(""); err != nil || path != filepath.Join(home, ".clowder", "config") {
		t.Errorf("home: %s, %v", path, err)
	}
}

func TestAConfigSurvivesItsFileReadableByTheUserAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".clowder", "config")
	config, err := Read(path)
	if err != nil || len(config.Contexts) != 0 {
		t.Fatalf("a missing file: %+v, %v", config, err)
	}
	if err := config.Add(testContext("prod")); err != nil {
		t.Fatal(err)
	}
	if err := config.Add(testContext("staging")); err != nil {
		t.Fatal(err)
	}
	if err := Write(path, config); err != nil {
		t.Fatal(err)
	}
	read, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if read.CurrentContext != "prod" || len(read.Contexts) != 2 || read.Contexts[1].Endpoints[1] != "203.0.113.11:7443" {
		t.Errorf("read back: %+v", read)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "current_context: prod") {
		t.Errorf("the keys are not snake_case:\n%s", content)
	}
	if runtime.GOOS == "windows" {
		return // the user's profile protects the file there
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("the file's mode: %v, %v", info.Mode(), err)
	}
	if info, err := os.Stat(filepath.Dir(path)); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("the directory's mode: %v, %v", info.Mode(), err)
	}
}

func TestContextPicksTheCurrentOrTheNamedOne(t *testing.T) {
	var config Config
	if _, err := config.Context(""); !errors.Is(err, ErrNoCurrentContext) {
		t.Errorf("no contexts: %v", err)
	}
	for _, name := range []string{"prod", "staging"} {
		if err := config.Add(testContext(name)); err != nil {
			t.Fatal(err)
		}
	}
	if current, err := config.Context(""); err != nil || current.Name != "prod" {
		t.Errorf("current: %+v, %v", current, err)
	}
	if named, err := config.Context("staging"); err != nil || named.Name != "staging" {
		t.Errorf("named: %+v, %v", named, err)
	}
	if _, err := config.Context("test"); !errors.Is(err, ErrNoContext) {
		t.Errorf("a context that is not there: %v", err)
	}
	if err := config.Use("staging"); err != nil || config.CurrentContext != "staging" {
		t.Errorf("use: %s, %v", config.CurrentContext, err)
	}
	if err := config.Use("test"); err == nil || config.CurrentContext != "staging" {
		t.Errorf("use of a context that is not there: %s, %v", config.CurrentContext, err)
	}
}

func TestATakenNameNeedsAnother(t *testing.T) {
	var config Config
	if err := config.Add(testContext("prod")); err != nil {
		t.Fatal(err)
	}
	if err := config.Add(testContext("prod")); !errors.Is(err, ErrContextTaken) {
		t.Errorf("a second prod: %v", err)
	}
	if len(config.Contexts) != 1 {
		t.Errorf("contexts: %+v", config.Contexts)
	}
}

func TestPutReplacesAContextInItsPlaceAndDeleteRemovesIt(t *testing.T) {
	var config Config
	for _, name := range []string{"prod", "staging"} {
		if err := config.Add(testContext(name)); err != nil {
			t.Fatal(err)
		}
	}
	renewed := testContext("prod")
	renewed.Certificate = "renewed"
	if err := config.Put(renewed); err != nil {
		t.Fatal(err)
	}
	if len(config.Contexts) != 2 || config.Contexts[0].Certificate != "renewed" || config.CurrentContext != "prod" {
		t.Errorf("after the renewal: %+v", config)
	}
	if err := config.Delete("prod"); err != nil {
		t.Fatal(err)
	}
	if len(config.Contexts) != 1 || config.Contexts[0].Name != "staging" || config.CurrentContext != "" {
		t.Errorf("after the delete: %+v", config)
	}
	if err := config.Delete("prod"); !errors.Is(err, ErrNoContext) {
		t.Errorf("a second delete: %v", err)
	}
}

func TestAnEmptyFileIsAnEmptyConfig(t *testing.T) {
	for _, content := range []string{"", "\n", "# nothing yet\n"} {
		if config, err := Parse([]byte(content)); err != nil || len(config.Contexts) != 0 {
			t.Errorf("%q: %+v, %v", content, config, err)
		}
	}
}

func TestParseRefuses(t *testing.T) {
	for _, test := range []struct{ name, content, want string }{
		{"kubeconfig's spelling of a key", "current-context: prod\n", "current-context"},
		{"no endpoints", "contexts:\n- {name: prod, ca: c, certificate: c, key: k}\n", "endpoints"},
		{"no key", "contexts:\n- {name: prod, endpoints: [h], ca: c, certificate: c}\n", "key are required"},
		{"a bad endpoint", "contexts:\n- {name: prod, endpoints: ['a b'], ca: c, certificate: c, key: k}\n", `"a b"`},
		{
			"a port out of range", "contexts:\n- {name: prod, endpoints: ['h:99999'], ca: c, certificate: c, key: k}\n",
			`"h:99999"`,
		},
		{"twice", "contexts:\n- {name: p, endpoints: [h], ca: c, certificate: c, key: k}\n" +
			"- {name: p, endpoints: [h], ca: c, certificate: c, key: k}\n", "twice"},
		{"a current context not there", "current_context: test\n", "names no context"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Parse([]byte(test.content)); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Errorf("got %v, want %q", err, test.want)
			}
		})
	}
}
