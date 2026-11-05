package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/webcodingcat/clowder/internal/api"
)

// writeSecrets writes a file of two Secrets as secret encrypt writes them,
// with a comment between, and returns it.
func writeSecrets(t *testing.T, cluster string) string {
	t.Helper()
	var content strings.Builder
	for _, secret := range []struct{ id, value string }{{"shop/token", "s3cret"}, {"shop/binary", "a\x00b"}} {
		written, err := runWithInput(t, secret.value, "--cluster", cluster, "secret", "encrypt", secret.id)
		if err != nil {
			t.Fatal(err)
		}
		content.WriteString("# " + secret.id + ", kept as it is\n" + written + "---\n")
	}
	path := filepath.Join(t.TempDir(), "secrets.yaml")
	if err := os.WriteFile(path, []byte(content.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRekeyChangesOnlyTheValuesAndKeepsThem(t *testing.T) {
	cluster := startCluster(t)
	path := writeSecrets(t, cluster)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"secret", "rekey", "-f", filepath.Dir(path)},
		{"secret", "rekey", "-f", path, "--identity", recoveryKeyFile()},
	} {
		out, err := run(t, append([]string{"--cluster", cluster}, args...)...)
		if err != nil || !strings.Contains(out, "2 values rekeyed") {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) == string(before) {
		t.Error("the values were not encrypted anew")
	}
	if !strings.Contains(string(after), "# shop/token, kept as it is\n") {
		t.Errorf("the comment is gone:\n%s", after)
	}
	if strings.Count(string(after), "\n") != strings.Count(string(before), "\n") {
		t.Errorf("the lines changed in number:\n%s", after)
	}
	out, err := run(t, "--cluster", cluster, "secret", "decrypt", "--identity", recoveryKeyFile(), "-f", path)
	if want := "shop/token\ts3cret\nshop/binary\tbase64:YQBi\n"; err != nil || out != want {
		t.Errorf("decrypt: %v\n%q, want %q", err, out, want)
	}
}

// writeOutOfPlace writes the Secrets of writeSecrets with shop/token renamed
// to shop/copied, so its value stands out of place, and returns the file.
func writeOutOfPlace(t *testing.T, cluster string) string {
	t.Helper()
	path := writeSecrets(t, cluster)
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
	cluster := startCluster(t)
	path := writeOutOfPlace(t, cluster)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"secret", "rekey", "-f", path},
		{"secret", "rekey", "-f", path, "--identity", recoveryKeyFile()},
	} {
		_, err := run(t, append([]string{"--cluster", cluster}, args...)...)
		if err == nil || !strings.Contains(err.Error(), "the value is for shop/token, not for shop/copied") {
			t.Errorf("%v: %v", args, err)
		}
	}
	if after, err := os.ReadFile(path); err != nil || string(after) != string(before) {
		t.Errorf("the refused file changed: %v", err)
	}
}

func TestRekeyRefusesAValueOutsideASecret(t *testing.T) {
	cluster := startCluster(t)
	path := writeSecrets(t, cluster)
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	moved := strings.Replace(string(content), "kind: Secret", "kind: Volume", 1)
	if err := os.WriteFile(path, []byte(moved), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = run(t, "--cluster", cluster, "secret", "rekey", "-f", path)
	if err == nil || !strings.Contains(err.Error(), "!secret is allowed only in a Secret's spec.value") {
		t.Errorf("a value in a Volume: %v", err)
	}
	// cluster.yaml's values in a file of another name are taken for none
	block, err := runWithInput(t, "the token", "--cluster", cluster, "secret", "encrypt", "--cluster-yaml")
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "hetzner.yaml")
	if err := os.WriteFile(other, []byte("hetzner:\n  token: "+block), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = run(t, "--cluster", cluster, "secret", "rekey", "-f", other)
	if err == nil || !strings.Contains(err.Error(), "stands only in a Secret or in a file named cluster.yaml") {
		t.Errorf("a value in another file: %v", err)
	}
}

func TestDecryptNamesAValueOutOfPlaceAndFails(t *testing.T) {
	cluster := startCluster(t)
	path := writeOutOfPlace(t, cluster)
	out, err := runWithInput(t, "", "secret", "decrypt", "--identity", recoveryKeyFile(), "-f", path)
	if want := "shop/copied\terror: the value is for shop/token, not for shop/copied\n"; !strings.Contains(out, want) {
		t.Errorf("no %q in %q", want, out)
	}
	if want := "shop/binary\tbase64:YQBi\n"; !strings.Contains(out, want) {
		t.Errorf("the other value is not shown: no %q in %q", want, out)
	}
	if ExitCode(err) != 1 || !strings.Contains(err.Error(), "1 of 2 values did not open") {
		t.Errorf("a value out of place: %v", err)
	}
	if _, err := run(t, "secret", "decrypt", "-f", path); ExitCode(err) != 2 {
		t.Errorf("decrypt without a key: %v", err)
	}
}

func TestRekeyAndDecryptTakeClusterYAMLsValuesByPath(t *testing.T) {
	cluster := startCluster(t)
	var content strings.Builder
	content.WriteString("hetzner:\n")
	entries := []struct{ key, value string }{{"token", "the token"}, {"note", "error: not one"}, {"padded", " x "}}
	for _, entry := range entries {
		block, err := runWithInput(t, entry.value, "--cluster", cluster, "secret", "encrypt", "--cluster-yaml")
		if err != nil {
			t.Fatal(err)
		}
		content.WriteString("  " + entry.key + ": " + block)
	}
	path := filepath.Join(t.TempDir(), "cluster.yaml")
	if err := os.WriteFile(path, []byte(content.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := run(t, "--cluster", cluster, "secret", "rekey", "-f", path); err != nil || !strings.Contains(out, "3 values rekeyed") {
		t.Fatalf("rekey: %v\n%s", err, out)
	}
	out, err := runWithInput(t, "", "secret", "decrypt", "--identity", recoveryKeyFile(), "-f", path)
	// a value that starts like a marker, or with a space, shows in base64
	want := "hetzner.note\tbase64:ZXJyb3I6IG5vdCBvbmU=\nhetzner.padded\tbase64:IHgg\nhetzner.token\tthe token\n"
	if err != nil || out != want {
		t.Errorf("decrypt: %v\n%q, want %q", err, out, want)
	}
}

func TestRekeyByClusterSplitsTheRequestsAndCountsAcrossThem(t *testing.T) {
	cluster := startCluster(t)
	client, err := (&globals{}).connect()
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(writeSecrets(t, cluster))
	if err != nil {
		t.Fatal(err)
	}
	values, err := fileValues("secrets.yaml", content)
	if err != nil {
		t.Fatal(err)
	}
	placed := []api.PlacedValue{values[0].PlacedValue, values[1].PlacedValue}
	// a batch of one byte takes one value per request
	rekey := rekeyByCluster(client, 1)
	rekeyed, err := rekey(context.Background(), placed)
	if err != nil {
		t.Fatal(err)
	}
	if len(rekeyed) != 2 || rekeyed[0] == placed[0].Encrypted || rekeyed[1] == placed[1].Encrypted {
		t.Errorf("rekeyed: %d values, the same as before or not in order", len(rekeyed))
	}
	placed[1].For = "shop/copied"
	_, err = rekey(context.Background(), placed)
	if err == nil || !strings.Contains(err.Error(), "the request with values 2 to 2: value 1: the value is for shop/binary") {
		t.Errorf("a value out of place in the second request: %v", err)
	}
}
