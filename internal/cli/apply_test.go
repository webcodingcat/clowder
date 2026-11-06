package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// configurationYAML is a ClusterConfiguration with its floating IP; it
// names the Hetzner token in clowder-system/hetzner, key token.
func configurationYAML(floatingIP string) string {
	return `apiVersion: clowder/v1
kind: ClusterConfiguration
metadata: {name: cluster}
spec:
  hetzner: {tokenSecretRef: {name: hetzner, key: token}}
  privateNetwork: 10.0.0.0/16
  ingress: {floatingIP: ` + floatingIP + `}
  storageClasses: {fast: {pool: vg-fast/thin}}
`
}

// writeConfiguration writes a file of the Secret of the Hetzner token and
// the configuration below it, as secret encrypt writes the Secret, and
// returns it.
func writeConfiguration(t *testing.T, token, floatingIP string) string {
	t.Helper()
	secret, err := run(t, "secret", "encrypt", "clowder-system/hetzner", "--from-literal", "token="+token)
	if err != nil {
		t.Fatalf("secret encrypt: %v\n%s", err, secret)
	}
	path := filepath.Join(t.TempDir(), "configuration.yaml")
	if err := os.WriteFile(path, []byte(secret+"---\n"+configurationYAML(floatingIP)), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestANewHetznerTokenIsShownAndNeedsYesWithoutATerminal(t *testing.T) {
	startCluster(t)
	first := writeConfiguration(t, "the token", "203.0.113.11")
	// the test's stdin is no terminal, so only --yes confirms
	out, err := runWithInput(t, "", "apply", "-f", first)
	if err == nil || !strings.Contains(err.Error(), "the apply changes the Hetzner token: confirm it with --yes") ||
		!strings.Contains(out, "The new Hetzner token sees the servers of 1 of 1 hosts.") {
		t.Errorf("apply without --yes: %v\n%s", err, out)
	}
	if _, err := run(t, "get", "clusterconfiguration", "cluster"); err == nil {
		t.Error("the configuration went in without --yes")
	}
	out, err = runWithInput(t, "", "apply", "-f", first, "--yes")
	if err != nil || !strings.Contains(out, "Secret clowder-system/hetzner: applied") ||
		!strings.Contains(out, "ClusterConfiguration cluster: applied") {
		t.Fatalf("apply --yes: %v\n%s", err, out)
	}
	out, err = run(t, "cluster", "status")
	if err != nil || !strings.Contains(out, "cluster test, configuration generation 1,") {
		t.Errorf("cluster status: %v\n%s", err, out)
	}
	shown, err := run(t, "get", "clusterconfiguration", "cluster", "-o", "yaml")
	if err != nil || !strings.Contains(shown, "floatingIP: 203.0.113.11") {
		t.Errorf("get -o yaml: %v\n%s", err, shown)
	}
	// the same token, encrypted anew, in a changed configuration asks nothing
	moved := writeConfiguration(t, "the token", "203.0.113.12")
	out, err = runWithInput(t, "", "apply", "-f", moved)
	if err != nil || !strings.Contains(out, "Secret clowder-system/hetzner: unchanged") ||
		!strings.Contains(out, "ClusterConfiguration cluster: applied") || strings.Contains(out, "Hetzner token") {
		t.Errorf("apply of the same token: %v\n%s", err, out)
	}
	// a dry run asks nothing either, and shows what the token sees
	out, err = runWithInput(t, "", "apply", "--dry-run", "-f", writeConfiguration(t, "a new token", "203.0.113.12"))
	if err != nil || !strings.Contains(out, "Secret clowder-system/hetzner: applied (dry run)") ||
		!strings.Contains(out, "The new Hetzner token sees the servers of 1 of 1 hosts.") {
		t.Errorf("dry run of a new token: %v\n%s", err, out)
	}
}

func TestDiffEndsWithZeroOneOrTwo(t *testing.T) {
	startCluster(t)
	first := writeConfiguration(t, "the token", "203.0.113.11")
	out, err := run(t, "diff", "-f", first)
	if ExitCode(err) != 1 || !Silent(err) || out != "Secret clowder-system/hetzner: new\nClusterConfiguration cluster: new\n" {
		t.Errorf("diff before the apply: exit code %d (%v)\n%s", ExitCode(err), err, out)
	}
	if _, err := run(t, "apply", "-f", first, "--yes"); err != nil {
		t.Fatal(err)
	}
	out, err = run(t, "diff", "-f", writeConfiguration(t, "the token", "203.0.113.11"))
	if err != nil || out != "nothing would change\n" {
		t.Errorf("diff of the same: %v\n%s", err, out)
	}
	out, err = run(t, "diff", "-f", writeConfiguration(t, "a new token", "203.0.113.12"))
	want := "Secret clowder-system/hetzner: differs in data.token\nClusterConfiguration cluster: differs in spec.ingress\n"
	if ExitCode(err) != 1 || out != want {
		t.Errorf("diff of a change: exit code %d (%v)\n%q, want %q", ExitCode(err), err, out, want)
	}
	// the configuration alone names a Secret the cluster would not have
	alone := filepath.Join(t.TempDir(), "alone.yaml")
	if err := os.WriteFile(alone, []byte(strings.Replace(configurationYAML("203.0.113.11"), "name: hetzner", "name: other", 1)),
		0o600); err != nil {
		t.Fatal(err)
	}
	out, err = run(t, "diff", "-f", alone)
	if ExitCode(err) != 2 || Silent(err) || !strings.Contains(out, "ClusterConfiguration cluster: refused") {
		t.Errorf("diff of a refused configuration: exit code %d (%v)\n%s", ExitCode(err), err, out)
	}
	if _, err := run(t, "diff", "-f", filepath.Join(t.TempDir(), "missing.yaml")); ExitCode(err) != 2 {
		t.Errorf("diff of a missing file: %v", err)
	}
}

func TestDiffsAnswersHaveTheirExitCodes(t *testing.T) {
	for _, test := range []struct {
		err    error
		code   int
		silent bool
	}{
		{nil, 0, false},
		{errDiffers, 1, true},
		{diffFailed{errors.New("no leader")}, 2, false},
		{errors.New("refused"), 1, false},
	} {
		if ExitCode(test.err) != test.code || Silent(test.err) != test.silent {
			t.Errorf("%v: exit code %d, silent %t; want %d, %t", test.err, ExitCode(test.err), Silent(test.err),
				test.code, test.silent)
		}
	}
}
