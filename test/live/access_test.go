//go:build live

package live

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// recoverAccess loses the test's credentials and gets new ones from root on
// the host, as after a stolen laptop: with --revoke-older the old
// certificate, as a thief would still hold it, is refused from then on.
func recoverAccess(t *testing.T, server host) {
	stolen, err := os.ReadFile(credentialsFile())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(credentialsFile()); err != nil {
		t.Fatal(err)
	}
	if _, err := clowder("cluster", "status"); err == nil {
		t.Fatal("the CLI reached the cluster without credentials")
	}
	// stdout alone: must's output has the command's stderr too
	issued := server.must(t, "clowderd credentials --revoke-older 2>/dev/null")
	writeCredentials(t, []byte(issued))
	mustClowder(t, "cluster", "status")

	// the host that issued them refuses the old certificate at once, the
	// others once they applied the cutoff: the CLI may ask any of them
	writeCredentials(t, stolen)
	eventually(t, 30*time.Second, "the old certificate refused", func() error {
		if _, err := clowder("cluster", "status"); err == nil || !strings.Contains(err.Error(), "revoke-older") {
			return fmt.Errorf("the old certificate after --revoke-older: %w", err)
		}
		return nil
	})
	writeCredentials(t, []byte(issued))
	mustClowder(t, "cluster", "status")
}

// rotateRecoveryKey replaces the recovery key: the new key counts, the
// files encrypted anew open with it and no longer with the old one, and the
// cluster keeps what it had.
func rotateRecoveryKey(t *testing.T, server host) {
	oldKey, newKey := filepath.Join(workDir, "recovery-key"), filepath.Join(workDir, "recovery-key-rotated")
	// --recovery-out writes no file over another: gone before, so a step
	// run again starts as the first time
	if err := os.Remove(newKey); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	mustClowder(t, "recovery", "rotate", "--recovery-out", newKey)
	rekeyAndDecrypt(t, server, newKey)
	if output, err := clowder("secret", "decrypt", "--identity", oldKey, "-f", stackFile("shop")); err == nil {
		t.Errorf("the old recovery key still opens the stack's file:\n%s", output)
	}
	// the new key takes the old one's place, where the next run of
	// TestStacks looks for it
	if err := os.Rename(newKey, oldKey); err != nil {
		t.Fatal(err)
	}
}

// writeCredentials puts the test's credentials in place, as the CLI writes
// them.
func writeCredentials(t *testing.T, content []byte) {
	t.Helper()
	if err := os.WriteFile(credentialsFile(), content, 0o600); err != nil {
		t.Fatal(err)
	}
}
