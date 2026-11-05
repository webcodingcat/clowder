package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/webcodingcat/clowder/internal/clientconfig"
)

// userConfig reads the credentials file startCluster wrote.
func userConfig(t *testing.T) clientconfig.Config {
	t.Helper()
	config, err := clientconfig.Read(filepath.Join(os.Getenv("HOME"), ".clowder", "config"))
	if err != nil {
		t.Fatal(err)
	}
	return config
}

// printed is the context test as clowderd credentials prints it, under
// another name.
func printed(t *testing.T, name string) string {
	t.Helper()
	cliContext, err := userConfig(t).Context("test")
	if err != nil {
		t.Fatal(err)
	}
	cliContext.Name = name
	encoded, err := yaml.Marshal(clientconfig.Config{CurrentContext: name, Contexts: []clientconfig.Context{cliContext}})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestImportAddsAContextUnderANameNotTaken(t *testing.T) {
	startCluster(t)
	if _, err := runWithInput(t, printed(t, "test"), "config", "import", "-"); err == nil ||
		!strings.Contains(err.Error(), "--as") || !strings.Contains(err.Error(), "--replace") {
		t.Errorf("an import under a name taken: %v", err)
	}
	out, err := runWithInput(t, printed(t, "test"), "config", "import", "-", "--as", "staging")
	if err != nil || !strings.Contains(out, "imported context staging") || !strings.Contains(out, "current: test") {
		t.Fatalf("import: %q, %v", out, err)
	}
	// from a file too, under the cluster's name
	file := filepath.Join(t.TempDir(), "credentials")
	if err := os.WriteFile(file, []byte(printed(t, "prod")), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runWithInput(t, "", "config", "import", file); err != nil {
		t.Fatal(err)
	}
	out, err = run(t, "config", "get-contexts")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if err != nil || len(lines) != 4 || !strings.HasPrefix(lines[1], "*") || !strings.Contains(lines[3], "prod") ||
		!strings.Contains(lines[1], time.Now().Add(365*24*time.Hour).Format(time.DateOnly)) {
		t.Errorf("get-contexts:\n%s %v", out, err)
	}
	if _, err := runWithInput(t, "not: credentials\n", "config", "import", "-"); err == nil {
		t.Error("an import of something else")
	}
}

func TestRenewedCredentialsReplaceTheOldAndAContextGoes(t *testing.T) {
	startCluster(t)
	renewed := strings.Replace(printed(t, "test"), "127.0.0.1:", "127.0.0.3:", 1)
	if _, err := runWithInput(t, renewed, "config", "import", "-", "--replace"); err != nil {
		t.Fatal(err)
	}
	config := userConfig(t)
	if len(config.Contexts) != 1 || !strings.HasPrefix(config.Contexts[0].Endpoints[0], "127.0.0.3:") ||
		config.CurrentContext != "test" {
		t.Errorf("after --replace: %+v", config)
	}
	out, err := run(t, "config", "delete-context", "test")
	if err != nil || !strings.Contains(out, "deleted context test") || !strings.Contains(out, "current: none") {
		t.Fatalf("delete-context: %q, %v", out, err)
	}
	if _, err := run(t, "host", "list"); err == nil || !strings.Contains(err.Error(), "use-context") {
		t.Errorf("without a current context: %v", err)
	}
	if _, err := run(t, "config", "delete-context", "test"); err == nil || !strings.Contains(err.Error(), "get-contexts") {
		t.Errorf("a context that is gone: %v", err)
	}
}

func TestTheContextPicksTheCluster(t *testing.T) {
	startCluster(t)
	broken := printed(t, "broken")
	broken = strings.Replace(broken, "127.0.0.1:", "127.0.0.3:", 1) // no agent there
	if _, err := runWithInput(t, broken, "config", "import", "-"); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "--connect-timeout", "1s", "--context", "broken", "host", "list"); err == nil {
		t.Error("the context broken reached the cluster")
	}
	if _, err := run(t, "host", "list"); err != nil {
		t.Errorf("the current context: %v", err)
	}
	if _, err := run(t, "config", "use-context", "missing"); err == nil {
		t.Error("use-context of a context that is not there")
	}
	if out, err := run(t, "config", "use-context", "broken"); err != nil || !strings.Contains(out, "switched to context broken") {
		t.Fatalf("use-context: %q, %v", out, err)
	}
	if userConfig(t).CurrentContext != "broken" {
		t.Errorf("the current context is %s", userConfig(t).CurrentContext)
	}
	if _, err := run(t, "--context", "test", "host", "list"); err != nil {
		t.Errorf("--context test: %v", err)
	}
}

func TestTheConfigFlagAndTheEnvironmentNameTheFile(t *testing.T) {
	startCluster(t)
	empty := filepath.Join(t.TempDir(), "config")
	if _, err := run(t, "--config", empty, "host", "list"); err == nil || !strings.Contains(err.Error(), "clowderd credentials") {
		t.Errorf("--config with no file there: %v", err)
	}
	t.Setenv(clientconfig.EnvVar, empty)
	if _, err := run(t, "host", "list"); err == nil {
		t.Errorf("$%s with no file there reached the cluster", clientconfig.EnvVar)
	}
}

func TestACertificateNearItsEndWarns(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name     string
		notAfter time.Time
		want     string
	}{
		{"far off", now.Add(renewWarning + time.Hour), ""},
		{"soon", now.Add(10 * 24 * time.Hour), "ends on 2026-11-15, in 10 days"},
		{"within a day", now.Add(time.Hour), "ends within a day, on 2026-11-05"},
		{"over", now.Add(-time.Hour), "ended on 2026-11-05"},
	} {
		var out bytes.Buffer
		warnBeforeTheEnd(&out, "prod", test.notAfter, now)
		switch {
		case test.want == "" && out.Len() != 0:
			t.Errorf("%s: a warning: %s", test.name, out.String())
		case test.want != "" && (!strings.Contains(out.String(), test.want) ||
			!strings.Contains(out.String(), "--as prod --replace")):
			t.Errorf("%s: %q, want %q", test.name, out.String(), test.want)
		}
	}
}
