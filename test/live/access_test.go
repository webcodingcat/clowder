//go:build live

package live

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// recoverAccess loses the test's credentials and gets new ones with a code
// from the host, as after a stolen laptop: the old certificate, as a thief
// would still hold it, is refused from then on, and so is the same code.
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
	code := recoveryCode(t, server, "tester", "clowder user recover tester")
	mustClowder(t, "user", "recover", "tester", "--code", code)
	mustClowder(t, "cluster", "status")

	recovered, err := os.ReadFile(credentialsFile())
	if err != nil {
		t.Fatal(err)
	}
	// whoever saw the code cannot take the user's place with it: the seed
	// refuses it if its state has the use already, else the leader's state
	// does (a seed that follows may be a moment behind)
	if output, err := clowder("user", "recover", "tester", "--code", code); err == nil ||
		!strings.Contains(output, "the code does not fit") && !strings.Contains(output, "the code was used already") {
		t.Errorf("the same code once more: %v\n%s", err, output)
	}
	writeCredentials(t, stolen)
	_, err = clowder("cluster", "status")
	writeCredentials(t, recovered)
	if err == nil || !strings.Contains(err.Error(), "is revoked") {
		t.Errorf("the old certificate after the recovery: %v", err)
	}
	mustClowder(t, "cluster", "status")
}

// rotateRecoveryKey replaces the recovery key: credentials alone, with a
// code that is not the user's, are refused; with the user's own code the
// new key counts. The files encrypted anew open with the new key and no
// longer with the old one, and the cluster keeps what it had.
func rotateRecoveryKey(t *testing.T, server host) {
	oldKey, newKey := filepath.Join(workDir, "recovery-key"), filepath.Join(workDir, "recovery-key-rotated")
	// --recovery-out writes no file over another, and the refused rotation
	// below leaves its key behind: gone before, gone after, so a step run
	// again starts as the first time
	refusedKey := filepath.Join(workDir, "recovery-key-refused")
	for _, leftOver := range []string{refusedKey, newKey} {
		if err := os.Remove(leftOver); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = os.Remove(refusedKey) })
	// a real code, for another user: the credentials are valid, the code not theirs
	output, err := clowder("recovery", "rotate", "--code",
		recoveryCode(t, server, "someone-else", "clowder recovery rotate"),
		"--recovery-out", refusedKey)
	if err == nil || !strings.Contains(output, "the code does not fit") {
		t.Fatalf("recovery rotate with another user's code: %v", err)
	}
	mustClowder(t, "recovery", "rotate", "--code", recoveryCode(t, server, "tester", "clowder recovery rotate"),
		"--recovery-out", newKey)
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

// recoveryCode is the user's code of now for the command that takes it
// ("clowder user recover <user>" or "clowder recovery rotate"), as root on
// the host makes it.
func recoveryCode(t *testing.T, server host, user, command string) string {
	t.Helper()
	output := server.must(t, "clowderd user recovery-code "+user)
	_, code, found := strings.Cut(output, command+" --code ")
	if !found {
		t.Fatalf("no code for %q:\n%s", command, output)
	}
	code, _, _ = strings.Cut(code, "\n")
	return code
}

// writeCredentials puts the test's credentials in place, as the CLI writes
// them.
func writeCredentials(t *testing.T, content []byte) {
	t.Helper()
	if err := os.WriteFile(credentialsFile(), content, 0o600); err != nil {
		t.Fatal(err)
	}
}
