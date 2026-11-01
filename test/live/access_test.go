//go:build live

package live

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// recoverAccess loses the test's credentials and gets new ones with a code
// from the host, as after a stolen laptop: the old certificate, as a thief
// would still hold it, is refused from then on.
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
	mustClowder(t, "user", "recover", "tester", "--code", recoveryCode(t, server, "tester"))
	mustClowder(t, "cluster", "status")

	recovered, err := os.ReadFile(credentialsFile())
	if err != nil {
		t.Fatal(err)
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
	// a real code, for another user: the credentials are valid, the code not theirs
	output, err := clowder("recovery", "rotate", "--code", recoveryCode(t, server, "someone-else"),
		"--recovery-out", filepath.Join(workDir, "recovery-key-refused"))
	if err == nil || !strings.Contains(output, "the code does not fit") {
		t.Fatalf("recovery rotate with another user's code: %v", err)
	}
	mustClowder(t, "recovery", "rotate", "--code", recoveryCode(t, server, "tester"), "--recovery-out", newKey)
	rekeyAndDecrypt(t, server, newKey)
	if output, err := clowder("secret", "decrypt", "--identity", oldKey, "-f", stackFile("shop")); err == nil {
		t.Errorf("the old recovery key still opens the stack's file:\n%s", output)
	}
}

// recoveryCode is the user's code of now, as root on the host makes it.
func recoveryCode(t *testing.T, server host, user string) string {
	t.Helper()
	output := server.must(t, "clowderd user recovery-code "+user)
	code, _, _ := strings.Cut(output, "\n")
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
