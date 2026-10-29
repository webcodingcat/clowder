package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/webcodingcat/clowder/internal/compose"
	"github.com/webcodingcat/clowder/internal/sopsage"
)

// stackWithSecrets loads the stack "shop" whose app asks for db-password,
// next to a secrets file with this plaintext, encrypted for a key the
// environment holds.
func stackWithSecrets(t *testing.T, plaintext string) (*compose.Stack, string) {
	t.Helper()
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SOPS_AGE_KEY", identity.String())
	t.Setenv("SOPS_AGE_KEY_FILE", "")
	dir := t.TempDir()
	sealed, err := sopsage.Encrypt([]byte(plaintext), []string{identity.Recipient().String()}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "secrets.sops.yaml"), sealed, 0o600); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "clowder.yaml")
	if err := os.WriteFile(file, []byte("name: shop\nx-clowder: {secrets_file: secrets.sops.yaml}\n"+
		"services: {app: {image: x, x-clowder: {secrets: [db-password]}}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stack, err := compose.Load(context.Background(), compose.LoadOptions{Name: "shop", WorkingDir: dir, Files: []string{file}})
	if err != nil {
		t.Fatal(err)
	}
	return stack, dir
}

func TestTheStackTakesOnlyTheSecretsItAsksFor(t *testing.T) {
	stack, dir := stackWithSecrets(t, "db-password: \"s3cret$\"\nother: kept-here\n")
	values, err := readStackSecrets(stack, dir)
	if err != nil || len(values) != 1 || values["db-password"] != "s3cret$" {
		t.Errorf("values %v, %v", values, err)
	}
}

func TestSecretsThatCannotBeUsedAreRefusedWithoutTheirValue(t *testing.T) {
	for want, plaintext := range map[string]string{
		"has no secret db-password":       "other: x\n",
		"not a string; quote it":          "db-password: 123456\n",
		"is empty":                        "db-password: \"\"\n",
		"keeps note_unencrypted in clear": "db-password: x\nnote_unencrypted: visible\n",
	} {
		stack, dir := stackWithSecrets(t, plaintext)
		_, err := readStackSecrets(stack, dir)
		if err == nil || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "123456") {
			t.Errorf("want an error with %q, got %v", want, err)
		}
	}
}
