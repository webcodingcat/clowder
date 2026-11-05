package clusterdir

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// useConfigDir points the user's config directory at a temporary one, on
// every system: os.UserConfigDir reads $XDG_CONFIG_HOME on Linux, $HOME on
// macOS and %AppData% on Windows.
func useConfigDir(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	t.Setenv("AppData", dir)
}

func TestCredentialsComeBackByTheClustersName(t *testing.T) {
	useConfigDir(t)
	written := Credentials{CA: "CA-PEM", Certificate: "CERT-PEM", Key: "KEY-PEM"}
	if err := WriteCredentials("prod", written); err != nil {
		t.Fatal(err)
	}
	read, err := ReadCredentials("prod")
	if err != nil || *read != written {
		t.Fatalf("read %+v, %v", read, err)
	}
	path, err := CredentialsPath("prod")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join("clowder", "clusters", "prod", "credentials.yaml"); !strings.HasSuffix(path, want) {
		t.Errorf("path %s", path)
	}
	if runtime.GOOS != "windows" { // Windows has no such modes; the profile protects the file
		for file, want := range map[string]os.FileMode{path: 0o600, filepath.Dir(path): 0o700} {
			if info, err := os.Stat(file); err != nil || info.Mode().Perm() != want {
				t.Errorf("%s: %v, %v; want %v", file, info.Mode().Perm(), err, want)
			}
		}
	}
	if _, err := ReadCredentials("staging"); err == nil || !strings.Contains(err.Error(), "no credentials for cluster staging") {
		t.Errorf("credentials of another cluster: %v", err)
	}
}

func TestCredentialsNeedAClusterNameFitForADirectory(t *testing.T) {
	useConfigDir(t)
	for _, name := range []string{"", "../prod", "Prod", "a/b"} {
		if _, err := CredentialsPath(name); err == nil {
			t.Errorf("%q: no error", name)
		}
	}
}

func TestReadCredentialsRefusesABrokenFile(t *testing.T) {
	useConfigDir(t)
	path, err := CredentialsPath("prod")
	if err != nil {
		t.Fatal(err)
	}
	for content, want := range map[string]string{
		"ca: x\ncert: x\n": "cert",
		// credentials of a release that had users: get new ones from a host
		"user: alice\n": "field user not found",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadCredentials("prod"); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want %q", content, err, want)
		}
	}
}

func TestABootstrapWritesOverNothing(t *testing.T) {
	useConfigDir(t)
	dir := t.TempDir()
	if err := CheckBootstrap(dir, "prod", false); err != nil {
		t.Fatalf("an empty directory: %v", err)
	}
	if err := WriteCredentials("prod", Credentials{CA: "CA-PEM"}); err != nil {
		t.Fatal(err)
	}
	if err := CheckBootstrap(dir, "prod", false); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Errorf("credentials of the same name: %v", err)
	}
	if err := CheckBootstrap(dir, "prod", true); err != nil {
		t.Errorf("with --force: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ConfigFile), []byte("name: prod\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckBootstrap(dir, "prod", true); err == nil || !strings.Contains(err.Error(), ConfigFile) {
		t.Errorf("a cluster.yaml, even with --force: %v", err)
	}
}
