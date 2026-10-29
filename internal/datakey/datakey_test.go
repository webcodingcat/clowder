package datakey

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func newKey(t *testing.T) *Key {
	t.Helper()
	key, err := New()
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestSealedSecretsOpenOnlyWithTheirKey(t *testing.T) {
	key := newKey(t)
	sealed, err := key.Seal([]byte("hcloud-token"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(sealed), "hcloud-token") {
		t.Error("the sealed secret shows the secret")
	}
	opened, err := key.Open(sealed)
	if err != nil || string(opened) != "hcloud-token" {
		t.Errorf("opened %q, %v", opened, err)
	}
	if _, err := newKey(t).Open(sealed); err == nil {
		t.Error("another key opened the secret")
	}
	if _, err := key.Open(nil); err == nil {
		t.Error("nothing sealed opened")
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
	sealed, err := key.Seal([]byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	if opened, err := loaded.Open(sealed); err != nil || string(opened) != "secret" {
		t.Errorf("the loaded key opened %q, %v", opened, err)
	}
	if parsed, err := Parse(key.Text()); err != nil || parsed.Text() != key.Text() {
		t.Errorf("the text form does not read back: %v", err)
	}
}
