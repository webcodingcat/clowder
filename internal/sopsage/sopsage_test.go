package sopsage

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"go.yaml.in/yaml/v3"
)

// testRecipient is the public half of testdata/test-age-key.txt.
const testRecipient = "age1mp9raff3qauz3nychwmgsq0p5tv2ej8r5uq8e9pnz0dasrt3hu7s3qqhxh"

func testIdentities(t *testing.T) []age.Identity {
	t.Helper()
	content, err := os.ReadFile(filepath.Join("testdata", "test-age-key.txt"))
	if err != nil {
		t.Fatal(err)
	}
	identities, err := age.ParseIdentities(strings.NewReader(string(content)))
	if err != nil {
		t.Fatal(err)
	}
	return identities
}

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	content, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return content
}

// assertSameYAML compares two YAML documents by their decoded values.
func assertSameYAML(t *testing.T, got, want []byte) {
	t.Helper()
	var gotValue, wantValue any
	if err := yaml.Unmarshal(got, &gotValue); err != nil {
		t.Fatalf("decode %s: %v", got, err)
	}
	if err := yaml.Unmarshal(want, &wantValue); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

// The fixtures plain*.sops.yaml were written by the real sops 3.12.2.
func TestDecryptReadsFilesOfTheRealSops(t *testing.T) {
	for _, name := range []string{"plain.sops.yaml", "plain.mac-only.sops.yaml", "plain.encrypted-suffix.sops.yaml"} {
		t.Run(name, func(t *testing.T) {
			plaintext, err := Decrypt(readTestdata(t, name), testIdentities(t))
			if err != nil {
				t.Fatal(err)
			}
			assertSameYAML(t, plaintext, readTestdata(t, "plain.yaml"))
		})
	}
}

func TestLeftInClearNamesTheValuesSopsKeptInClear(t *testing.T) {
	inClear, err := LeftInClear(readTestdata(t, "plain.sops.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(inClear, ",") != "note_unencrypted" {
		t.Errorf("left in clear: %v", inClear)
	}
}

func TestEncryptThenDecryptGivesThePlaintextBack(t *testing.T) {
	sealed, err := Encrypt(readTestdata(t, "plain.yaml"), []string{testRecipient}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"note_unencrypted: visible", "empty: \"\"", "nothing: null", "type:bool]", "recipient: " + testRecipient} {
		if !strings.Contains(string(sealed), want) {
			t.Errorf("the encrypted file lacks %q:\n%s", want, sealed)
		}
	}
	if strings.Contains(string(sealed), "shop") {
		t.Errorf("a value is left in plaintext:\n%s", sealed)
	}
	plaintext, err := Decrypt(sealed, testIdentities(t))
	if err != nil {
		t.Fatal(err)
	}
	assertSameYAML(t, plaintext, readTestdata(t, "plain.yaml"))
}

func TestDecryptRefusesChangedFiles(t *testing.T) {
	sealed := string(readTestdata(t, "plain.sops.yaml"))
	nameLine, countLine := lineWith(t, sealed, "name: "), lineWith(t, sealed, "count: ")
	tests := []struct {
		name    string
		file    string
		wantErr string
	}{
		{"plaintext value changed", strings.Replace(sealed, "note_unencrypted: visible", "note_unencrypted: changed", 1), "MAC does not match"},
		{"value moved to another key", strings.Replace(sealed, countLine, "count: "+strings.TrimPrefix(nameLine, "name: "), 1), "does not open"},
		{"value removed", strings.Replace(sealed, nameLine+"\n", "", 1), "MAC does not match"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Decrypt([]byte(test.file), testIdentities(t))
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Errorf("want an error with %q, got %v", test.wantErr, err)
			}
		})
	}
}

// lineWith returns the first line of text that starts with prefix.
func lineWith(t *testing.T, text, prefix string) string {
	t.Helper()
	for line := range strings.Lines(text) {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSuffix(line, "\n")
		}
	}
	t.Fatalf("no line starts with %q", prefix)
	return ""
}

func TestDecryptNeedsAMatchingIdentity(t *testing.T) {
	other, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	_, err = Decrypt(readTestdata(t, "plain.sops.yaml"), []age.Identity{other})
	if err == nil || !strings.Contains(err.Error(), "none of the age identities") {
		t.Errorf("want a missing-identity error, got %v", err)
	}
}

func TestDecryptRefusesWhatItDoesNotImplement(t *testing.T) {
	sealed := string(readTestdata(t, "plain.sops.yaml"))
	tests := map[string]string{
		"not SOPS":   "name: shop\n",
		"regex":      strings.Replace(sealed, "    unencrypted_suffix: _unencrypted", "    encrypted_regex: ^data$", 1),
		"key groups": strings.Replace(sealed, "    unencrypted_suffix: _unencrypted", "    key_groups:\n        - age: []", 1),
	}
	for name, file := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Decrypt([]byte(file), testIdentities(t)); err == nil {
				t.Error("want an error")
			}
		})
	}
}

func TestEncryptRefusesBadInput(t *testing.T) {
	tests := map[string]struct {
		plaintext  string
		recipients []string
	}{
		"no recipient":      {"a: b\n", nil},
		"bad recipient":     {"a: b\n", []string{"age1nope"}},
		"not a mapping":     {"- a\n", []string{testRecipient}},
		"already encrypted": {"a: b\nsops: {}\n", []string{testRecipient}},
		"alias":             {"a: &x b\nc: *x\n", []string{testRecipient}},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Encrypt([]byte(test.plaintext), test.recipients, time.Now()); err == nil {
				t.Error("want an error")
			}
		})
	}
}

func TestLoadIdentitiesReadsTheKeyFileFromTheEnvironment(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("SOPS_AGE_SSH_PRIVATE_KEY_FILE", "")
	t.Setenv("SOPS_AGE_KEY", "")
	key, err := filepath.Abs(filepath.Join("testdata", "test-age-key.txt"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SOPS_AGE_KEY_FILE", key)
	identities, err := LoadIdentities()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decrypt(readTestdata(t, "plain.sops.yaml"), identities); err != nil {
		t.Error(err)
	}

	t.Setenv("SOPS_AGE_KEY_FILE", filepath.Join(home, "missing.txt"))
	if _, err := LoadIdentities(); err == nil {
		t.Error("want an error for a named key file that is missing")
	}
}
