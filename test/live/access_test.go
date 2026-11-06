//go:build live

package live

import (
	"fmt"
	"os"
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

// writeCredentials puts the test's credentials in place, as the CLI writes
// them.
func writeCredentials(t *testing.T, content []byte) {
	t.Helper()
	if err := os.WriteFile(credentialsFile(), content, 0o600); err != nil {
		t.Fatal(err)
	}
}
