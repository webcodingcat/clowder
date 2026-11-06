package datakey

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/webcodingcat/clowder/internal/state"
)

func newKey(t *testing.T) *Key {
	t.Helper()
	key, err := New()
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestEncryptedSecretsDecryptOnlyWithTheirKey(t *testing.T) {
	key := newKey(t)
	encrypted, err := key.Encrypt([]byte("hcloud-token"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encrypted), "hcloud-token") {
		t.Error("the encrypted secret shows the secret")
	}
	decrypted, err := key.Decrypt(encrypted)
	if err != nil || string(decrypted) != "hcloud-token" {
		t.Errorf("decrypted %q, %v", decrypted, err)
	}
	if _, err := newKey(t).Decrypt(encrypted); err == nil {
		t.Error("another key decrypted the secret")
	}
	if _, err := key.Decrypt(nil); err == nil {
		t.Error("nothing encrypted decrypted")
	}
}

// The state works out a value's length from its encrypted length without
// importing age; this test, the one place that encrypts for real, holds the
// two together.
func TestTheStateReadsHowLongAnEncryptedValueIsInClear(t *testing.T) {
	key := newKey(t)
	for _, size := range []int{1, 100, 64<<10 - 1, 64 << 10, 64<<10 + 1, 200_000} {
		encrypted, err := key.Encrypt([]byte(strings.Repeat("x", size)))
		if err != nil {
			t.Fatal(err)
		}
		if got := state.ClearSize(encrypted); got != size {
			t.Errorf("%d bytes encrypted: state.ClearSize says %d", size, got)
		}
	}
}

func TestDerivedKeysDependOnTheKeyAndThePurpose(t *testing.T) {
	key := newKey(t)
	derive := func(key *Key, purpose string) string {
		t.Helper()
		derived, err := key.Derive(purpose)
		if err != nil || len(derived) != 32 {
			t.Fatalf("derived %d bytes, %v", len(derived), err)
		}
		return string(derived)
	}
	again, err := Parse(key.Text())
	if err != nil {
		t.Fatal(err)
	}
	if derive(key, "a") != derive(again, "a") {
		t.Error("the same key and purpose gave two keys")
	}
	if derive(key, "a") == derive(key, "b") || derive(key, "a") == derive(newKey(t), "a") {
		t.Error("another purpose or key gave the same key")
	}
}

func TestTheKeySurvivesItsFile(t *testing.T) {
	key := newKey(t)
	path := filepath.Join(t.TempDir(), "keys", "data-key.txt")
	if err := key.Save(path); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("the key file has mode %v, want 0600", info.Mode().Perm())
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := key.Encrypt([]byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	if decrypted, err := loaded.Decrypt(encrypted); err != nil || string(decrypted) != "secret" {
		t.Errorf("the loaded key decrypted %q, %v", decrypted, err)
	}
	if parsed, err := Parse(key.Text()); err != nil || parsed.Text() != key.Text() {
		t.Errorf("the text form does not read back: %v", err)
	}
}
