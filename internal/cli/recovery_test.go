package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"

	"github.com/webcodingcat/clowder/internal/api"
)

func TestTheCLIRotatesTheRecoveryKey(t *testing.T) {
	dir := startCluster(t)
	keys := t.TempDir()
	out, err := run(t, "--cluster", dir, "recovery", "rotate", "--recovery-out", filepath.Join(keys, "new"))
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
	client, err := (&globals{}).connect()
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
