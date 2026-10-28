//go:build sops

// These tests run the real sops (bin/dev test-sops installs it): files this
// package writes must open with sops, and files sops writes with this package.
package sopsage

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// runSops runs sops with the test key and returns its output.
func runSops(t *testing.T, args ...string) []byte {
	t.Helper()
	key, err := filepath.Abs(filepath.Join("testdata", "test-age-key.txt"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sops", args...)
	cmd.Env = append(os.Environ(), "SOPS_AGE_KEY_FILE="+key)
	cmd.Dir = t.TempDir() // no .sops.yaml above it
	output, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			t.Fatalf("sops %v: %v\n%s", args, err, exitErr.Stderr)
		}
		t.Fatalf("sops %v: %v", args, err)
	}
	return output
}

func TestTheRealSopsDecryptsWhatEncryptWrites(t *testing.T) {
	sealed, err := Encrypt(readTestdata(t, "plain.yaml"), []string{testRecipient}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "secrets.sops.yaml")
	if err := os.WriteFile(file, sealed, 0o600); err != nil {
		t.Fatal(err)
	}
	plaintext := runSops(t, "decrypt", "--input-type", "yaml", "--output-type", "yaml", file)
	assertSameYAML(t, plaintext, readTestdata(t, "plain.yaml"))
}

func TestDecryptReadsWhatTheRealSopsWritesNow(t *testing.T) {
	plain, err := filepath.Abs(filepath.Join("testdata", "plain.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for name, extra := range map[string][]string{
		"default":          nil,
		"encrypted suffix": {"--encrypted-suffix", "_secret"},
	} {
		t.Run(name, func(t *testing.T) {
			args := append([]string{"encrypt", "--age", testRecipient, "--input-type", "yaml", "--output-type", "yaml"}, extra...)
			sealed := runSops(t, append(args, plain)...)
			plaintext, err := Decrypt(sealed, testIdentities(t))
			if err != nil {
				t.Fatal(err)
			}
			assertSameYAML(t, plaintext, readTestdata(t, "plain.yaml"))
		})
	}
}
