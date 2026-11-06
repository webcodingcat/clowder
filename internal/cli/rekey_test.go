package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
)

// writeSealedSecrets writes a file of two SealedSecrets as secret seal
// writes them, with a comment between, and returns it.
func writeSealedSecrets(t *testing.T) string {
	t.Helper()
	var content strings.Builder
	for _, secret := range []struct{ id, value string }{{"shop/token", "s3cret"}, {"shop/binary", "a\x00b"}} {
		written, err := runWithInput(t, secret.value, "secret", "seal", secret.id, "--from-file", "value=-")
		if err != nil {
			t.Fatal(err)
		}
		content.WriteString("# " + secret.id + ", kept as it is\n" + written + "---\n")
	}
	return writeFile(t, "secrets.yaml", content.String())
}

// newSealingKey writes a new sealing key and its public half to files, as
// clowderd sealing-key export writes them, and returns their paths.
func newSealingKey(t *testing.T) (identityFile, certFile string) {
	t.Helper()
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	return writeFile(t, "new-key", identity.String()+"\n"), writeFile(t, "new-key.pub", identity.Recipient().String()+"\n")
}

func TestRekeyMovesTheValuesToAnotherKeyAndChangesOnlyThem(t *testing.T) {
	startCluster(t)
	path := writeSealedSecrets(t)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	newKey, newCert := newSealingKey(t)
	out, err := run(t, "secret", "rekey", "-f", filepath.Dir(path), "--identity", sealingKeyFile(), "--cert", newCert)
	if err != nil || !strings.Contains(out, "2 values rekeyed") {
		t.Fatalf("rekey: %v\n%s", err, out)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), "# shop/token, kept as it is\n") ||
		strings.Count(string(after), "\n") != strings.Count(string(before), "\n") {
		t.Errorf("more than the values changed:\n%s", after)
	}
	if _, err := run(t, "secret", "decrypt", "--identity", sealingKeyFile(), "-f", path); err == nil {
		t.Error("the old key still opens the values")
	}
	out, err = run(t, "secret", "decrypt", "--identity", newKey, "-f", path)
	if want := "shop/token[value]\ts3cret\nshop/binary[value]\tbase64:YQBi\n"; err != nil || out != want {
		t.Errorf("decrypt: %v\n%q, want %q", err, out, want)
	}
	// back to the cluster of the context, without --cert
	if out, err := run(t, "secret", "rekey", "-f", path, "--identity", newKey); err != nil ||
		!strings.Contains(out, "2 values rekeyed") {
		t.Fatalf("rekey for the cluster: %v\n%s", err, out)
	}
	if _, err := run(t, "secret", "decrypt", "--identity", sealingKeyFile(), "-f", path); err != nil {
		t.Errorf("the cluster's key does not open the values: %v", err)
	}
	if _, err := run(t, "secret", "rekey", "-f", path); ExitCode(err) != 2 {
		t.Errorf("rekey without the old key: %v", err)
	}
}

// writeOutOfPlace writes the SealedSecrets of writeSealedSecrets with
// shop/token renamed to shop/copied, so its value stands out of place, and
// returns the file.
func writeOutOfPlace(t *testing.T) string {
	t.Helper()
	path := writeSealedSecrets(t)
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	renamed := strings.Replace(string(content), "name: token}", "name: copied}", 1)
	if err := os.WriteFile(path, []byte(renamed), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRekeyRefusesAValueOutOfPlace(t *testing.T) {
	startCluster(t)
	path := writeOutOfPlace(t)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = run(t, "secret", "rekey", "-f", path, "--identity", sealingKeyFile())
	if err == nil || !strings.Contains(err.Error(), "the value is sealed for shop/token, not for shop/copied") {
		t.Errorf("rekey: %v", err)
	}
	if after, err := os.ReadFile(path); err != nil || string(after) != string(before) {
		t.Errorf("the refused file changed: %v", err)
	}
}

func TestDecryptNamesAValueOutOfPlaceAndFails(t *testing.T) {
	startCluster(t)
	path := writeOutOfPlace(t)
	out, err := runWithInput(t, "", "secret", "decrypt", "--identity", sealingKeyFile(), "-f", path)
	if want := "shop/copied[value]\terror: the value is sealed for shop/token, not for shop/copied\n"; !strings.Contains(out, want) {
		t.Errorf("no %q in %q", want, out)
	}
	if want := "shop/binary[value]\tbase64:YQBi\n"; !strings.Contains(out, want) {
		t.Errorf("the other value is not shown: no %q in %q", want, out)
	}
	if ExitCode(err) != 1 || !strings.Contains(err.Error(), "1 of 2 values did not open") {
		t.Errorf("a value out of place: %v", err)
	}
	if _, err := run(t, "secret", "decrypt", "-f", path); ExitCode(err) != 2 {
		t.Errorf("decrypt without a key: %v", err)
	}
}

func TestDecryptShowsAValueThatReadsLikeAMarkerInBase64(t *testing.T) {
	startCluster(t)
	out, err := run(t, "secret", "seal", "clowder-system/alerts",
		"--from-literal", "token=the token", "--from-literal", "note=error: not one", "--from-literal", "padded= x ")
	if err != nil {
		t.Fatal(err)
	}
	out, err = runWithInput(t, "", "secret", "decrypt", "--identity", sealingKeyFile(), "-f", writeFile(t, "alerts.yaml", out))
	// a value that starts like a marker, or with a space, shows in base64
	want := "clowder-system/alerts[note]\tbase64:ZXJyb3I6IG5vdCBvbmU=\nclowder-system/alerts[padded]\tbase64:IHgg\n" +
		"clowder-system/alerts[token]\tthe token\n"
	if err != nil || out != want {
		t.Errorf("decrypt: %v\n%q, want %q", err, out, want)
	}
}
