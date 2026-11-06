package cli

import (
	"encoding/json"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/webcodingcat/clowder/internal/resource"
)

func TestEncryptPrintsASecretThatApplyReads(t *testing.T) {
	startCluster(t)
	file := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(file, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, "secret", "encrypt", "shop/db", "--from-literal", "username=robot", "--from-file", "password="+file)
	if err != nil {
		t.Fatalf("secret encrypt: %v\n%s", err, out)
	}
	if strings.Contains(out, "s3cret") || strings.Contains(out, "robot") {
		t.Errorf("a value in clear:\n%s", out)
	}
	objects, err := resource.ParseDocuments([]byte(out))
	if err != nil || len(objects) != 1 || objects[0].Kind != resource.KindSecret ||
		objects[0].Metadata.Namespace != "shop" || objects[0].Metadata.Name != "db" {
		t.Fatalf("read back: %+v, %v\n%s", objects, err, out)
	}
	var spec resource.SecretSpec
	if err := json.Unmarshal(objects[0].Spec, &spec); err != nil ||
		!slices.Equal(slices.Sorted(maps.Keys(spec.Encrypted)), []string{"password", "username"}) {
		t.Errorf("spec %s, %v", objects[0].Spec, err)
	}
	// a file's value is taken as it is, with its line end
	path := filepath.Join(t.TempDir(), "db.yaml")
	if err := os.WriteFile(path, []byte(out), 0o600); err != nil {
		t.Fatal(err)
	}
	decrypted, err := run(t, "secret", "decrypt", "--identity", recoveryKeyFile(), "-f", path)
	if want := "shop/db[password]\tbase64:czNjcmV0Cg==\nshop/db[username]\trobot\n"; err != nil || decrypted != want {
		t.Errorf("decrypt: %v\n%q, want %q", err, decrypted, want)
	}
	// without a namespace, default
	if out, err := run(t, "secret", "encrypt", "db", "--from-literal", "value=x"); err != nil ||
		!strings.Contains(out, "metadata: {namespace: default, name: db}") {
		t.Errorf("secret encrypt without a namespace: %v\n%s", err, out)
	}
}

func TestEncryptRefusesWrongValuesAsAWrongCall(t *testing.T) {
	startCluster(t)
	for _, args := range [][]string{
		{"shop/db"}, // no value
		{"shop/db", "--from-literal", "value"},
		{"shop/db", "--from-literal", "Bad Key=x"},
		{"shop/db", "--from-literal", "value=x", "--from-literal", "value=y"},
		{"shop/db", "--from-file", "a=-", "--from-file", "b=-"},
		{"Shop/db", "--from-literal", "value=x"},
		{},
	} {
		if _, err := runWithInput(t, "x", append([]string{"secret", "encrypt"}, args...)...); ExitCode(err) != 2 {
			t.Errorf("%v: exit code %d (%v), want 2", args, ExitCode(err), err)
		}
	}
	if _, err := run(t, "secret", "encrypt", "shop/db", "--from-literal", "value="); err == nil ||
		!strings.Contains(err.Error(), "the value is empty") {
		t.Errorf("an empty value: %v", err)
	}
}

func TestAValueFromAPipeLosesOneLineEnd(t *testing.T) {
	for input, want := range map[string]string{
		"token\n":    "token",
		"token\r\n":  "token",
		"token":      "token",
		"token\n\n":  "token\n",
		"two\nlines": "two\nlines",
	} {
		value, err := readValue(strings.NewReader(input), io.Discard)
		if err != nil || string(value) != want {
			t.Errorf("%q: %q, %v", input, value, err)
		}
	}
	if _, err := readValue(strings.NewReader("\n"), io.Discard); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("an empty value: %v", err)
	}
}

func TestTheRecoveryKeyIsConfirmedByItsLastCharacters(t *testing.T) {
	key := "AGE-SECRET-KEY-1QQQQQQQQQQABCDEFGH"
	if err := confirmKey(strings.NewReader("wrong\n abcdefgh \n"), io.Discard, key); err != nil {
		t.Errorf("right at the second try, in lower case: %v", err)
	}
	if err := confirmKey(strings.NewReader("a\nb\nc\nABCDEFGH\n"), io.Discard, key); err == nil ||
		!strings.Contains(err.Error(), "nothing was sent") {
		t.Errorf("wrong three times: %v", err)
	}
	if err := confirmKey(strings.NewReader(""), io.Discard, key); err == nil {
		t.Error("no input confirmed the key")
	}
}
