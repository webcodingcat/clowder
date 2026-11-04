package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/webcodingcat/clowder/internal/api"
	"github.com/webcodingcat/clowder/internal/clusterdir"
	"github.com/webcodingcat/clowder/internal/recoverycode"
)

// codeOf is what clowderd user recovery-code gives startCluster's agent for
// a use, whose keys derive the purpose itself (ageKeys).
func codeOf(use recoverycode.Use, user string) string {
	return recoverycode.Make([]byte(recoverycode.Purpose), use, user, time.Now()).String()
}

func TestAUserRecoversWithACodeFromAHost(t *testing.T) {
	dir := startCluster(t)
	before, err := clusterdir.ReadCredentials("test")
	if err != nil {
		t.Fatal(err)
	}
	refused := map[string]string{
		"bob's code":             codeOf(recoverycode.ForRecover, "bob"),
		"alice's code to rotate": codeOf(recoverycode.ForRotate, "alice"),
	}
	for name, code := range refused {
		if _, err := run(t, "--cluster", dir, "user", "recover", "alice", "--code", code); err == nil ||
			!strings.Contains(err.Error(), "cannot prove it knows the code") {
			t.Errorf("%s: %v", name, err)
		}
	}

	out, err := run(t, "--cluster", dir, "user", "recover", "alice", "--code",
		strings.ToLower(codeOf(recoverycode.ForRecover, "alice")))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Replaced your credentials for cluster test") || !strings.Contains(out, "alice is back") {
		t.Errorf("user recover:\n%s", out)
	}
	after, err := clusterdir.ReadCredentials("test")
	if err != nil {
		t.Fatal(err)
	}
	if after.Certificate == before.Certificate || after.CA != before.CA {
		t.Error("the credentials are not the new certificate of the same CA")
	}
	if _, err := run(t, "--cluster", dir, "host", "list"); err != nil {
		t.Errorf("the new credentials: %v", err)
	}
}

func TestARecoveryWithoutAClusterDirectoryNeedsNameAndEndpoint(t *testing.T) {
	startCluster(t)
	t.Chdir(t.TempDir()) // no cluster.yaml here
	_, err := run(t, "user", "recover", "alice", "--code", codeOf(recoverycode.ForRecover, "alice"))
	if ExitCode(err) != 2 || !strings.Contains(err.Error(), "--name and --endpoint") {
		t.Errorf("want a wrong call that names --name and --endpoint, got %v", err)
	}
}

func TestOnlyYourOwnCodeRotatesTheRecoveryKey(t *testing.T) {
	dir := startCluster(t)
	keys := t.TempDir()
	_, err := run(t, "--cluster", dir, "recovery", "rotate", "--code", codeOf(recoverycode.ForRotate, "bob"),
		"--recovery-out", filepath.Join(keys, "refused"))
	if err == nil || !strings.Contains(err.Error(), "the old one still does") {
		t.Errorf("bob's code: %v", err)
	}

	out, err := run(t, "--cluster", dir, "recovery", "rotate", "--code", codeOf(recoverycode.ForRotate, "alice"),
		"--recovery-out", filepath.Join(keys, "new"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "secret rekey") {
		t.Errorf("recovery rotate says nothing of rekey:\n%s", out)
	}
	written, err := os.ReadFile(filepath.Join(keys, "new"))
	if err != nil {
		t.Fatal(err)
	}
	identity, err := age.ParseX25519Identity(strings.TrimSpace(string(written)))
	if err != nil {
		t.Fatal(err)
	}
	client, _, err := (&globals{cluster: dir}).connect()
	if err != nil {
		t.Fatal(err)
	}
	var encryption api.Encryption
	if err := client.Get(context.Background(), "/v1/encryption", &encryption); err != nil {
		t.Fatal(err)
	}
	if identity.Recipient().String() != encryption.Recovery {
		t.Errorf("the cluster's recovery key %s is not the one written", encryption.Recovery)
	}
}
