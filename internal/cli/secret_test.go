package cli

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/webcodingcat/clowder/internal/resource"
)

// writeFile writes content to a new file of the test and returns its path.
func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSealPrintsASealedSecretThatApplyReads(t *testing.T) {
	startCluster(t)
	file := writeFile(t, "password", "s3cret\n")
	out, err := run(t, "secret", "seal", "shop/db", "--from-literal", "username=robot", "--from-file", "password="+file)
	if err != nil {
		t.Fatalf("secret seal: %v\n%s", err, out)
	}
	if strings.Contains(out, "s3cret") || strings.Contains(out, "robot") {
		t.Errorf("a value in clear:\n%s", out)
	}
	objects, err := resource.ParseDocuments([]byte(out))
	if err != nil || len(objects) != 1 || objects[0].Kind != resource.KindSealedSecret ||
		objects[0].Metadata.Namespace != "shop" || objects[0].Metadata.Name != "db" {
		t.Fatalf("read back: %+v, %v\n%s", objects, err, out)
	}
	spec, err := resource.DecodeSealedSecretSpec(objects[0].Spec)
	if err != nil || len(spec.EncryptedData) != 2 {
		t.Errorf("spec %s, %v", objects[0].Spec, err)
	}
	// a file's value is taken as it is, with its line end
	decrypted, err := run(t, "secret", "decrypt", "--identity", sealingKeyFile(), "-f", writeFile(t, "db.yaml", out))
	if want := "shop/db[password]\tbase64:czNjcmV0Cg==\nshop/db[username]\trobot\n"; err != nil || decrypted != want {
		t.Errorf("decrypt: %v\n%q, want %q", err, decrypted, want)
	}
	// without a namespace, default
	if out, err := run(t, "secret", "seal", "db", "--from-literal", "value=x"); err != nil ||
		!strings.Contains(out, "metadata: {namespace: default, name: db}") {
		t.Errorf("secret seal without a namespace: %v\n%s", err, out)
	}
}

func TestSealTurnsTheSecretsOfAFileIntoSealedSecretsOffline(t *testing.T) {
	startCluster(t)
	secrets := writeFile(t, "secrets.yaml", "apiVersion: clowder/v1\nkind: Secret\nmetadata: {name: db}\n"+
		"spec: {data: {password: czNjcmV0}}\n---\n"+
		"apiVersion: clowder/v1\nkind: Secret\nmetadata: {namespace: shop, name: api}\nspec: {stringData: {token: t0k}}\n")
	// --cert needs no cluster: the context points nowhere
	if err := os.Remove(filepath.Join(os.Getenv("HOME"), ".clowder", "config")); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, "secret", "seal", "-f", secrets, "--cert", publicSealingKeyFile())
	if err != nil {
		t.Fatalf("secret seal -f: %v\n%s", err, out)
	}
	objects, err := resource.ParseDocuments([]byte(out))
	if err != nil || len(objects) != 2 || objects[0].Ref().String() != "SealedSecret default/db" ||
		objects[1].Ref().String() != "SealedSecret shop/api" {
		t.Fatalf("read back: %+v, %v\n%s", objects, err, out)
	}
	decrypted, err := run(t, "secret", "decrypt", "--identity", sealingKeyFile(), "-f", writeFile(t, "sealed.yaml", out))
	if want := "default/db[password]\ts3cret\nshop/api[token]\tt0k\n"; err != nil || decrypted != want {
		t.Errorf("decrypt: %v\n%q, want %q", err, decrypted, want)
	}
	volume := writeFile(t, "volume.yaml", "apiVersion: clowder/v1\nkind: Volume\nmetadata: {name: data}\n")
	if _, err := run(t, "secret", "seal", "-f", volume, "--cert", publicSealingKeyFile()); err == nil ||
		!strings.Contains(err.Error(), "Volume default/data: seal takes Secrets only") {
		t.Errorf("seal of a Volume: %v", err)
	}
}

func TestSealRefusesWrongValuesAsAWrongCall(t *testing.T) {
	startCluster(t)
	for _, args := range [][]string{
		{"shop/db"}, // no value
		{"shop/db", "--from-literal", "value"},
		{"shop/db", "--from-literal", "Bad Key=x"},
		{"shop/db", "--from-literal", "value=x", "--from-literal", "value=y"},
		{"shop/db", "--from-file", "a=-", "--from-file", "b=-"},
		{"Shop/db", "--from-literal", "value=x"},
		{"shop/db", "-f", "-"},
		{"-f", "-", "--from-literal", "value=x"},
		{},
	} {
		if _, err := runWithInput(t, "x", append([]string{"secret", "seal"}, args...)...); ExitCode(err) != 2 {
			t.Errorf("%v: exit code %d (%v), want 2", args, ExitCode(err), err)
		}
	}
	if _, err := run(t, "secret", "seal", "shop/db", "--from-literal", "value="); err == nil ||
		!strings.Contains(err.Error(), "the value is empty") {
		t.Errorf("an empty value: %v", err)
	}
}

func TestCreateSecretSendsTheValuesAndRefusesAnExistingSecret(t *testing.T) {
	startCluster(t)
	if _, err := run(t, "apply", "-f", writeFile(t, "ns.yaml", "apiVersion: clowder/v1\nkind: Namespace\nmetadata: {name: shop}\n")); err != nil {
		t.Fatal(err)
	}
	binary := writeFile(t, "binary", "a\x00\xffb")
	out, err := run(t, "create", "secret", "shop/db", "--from-literal", "username=robot", "--from-file", "key="+binary)
	if err != nil || out != "Secret shop/db: applied\n" {
		t.Fatalf("create secret: %v\n%s", err, out)
	}
	if out, err := run(t, "get", "secret", "shop/db"); err != nil || !strings.Contains(out, "key,username") {
		t.Errorf("get secret: %v\n%s", err, out)
	}
	out, err = run(t, "create", "secret", "shop/db", "--from-literal", "username=other")
	if err == nil || !strings.Contains(out, "Secret shop/db: refused: Secret shop/db already exists") {
		t.Errorf("a second create: %v\n%s", err, out)
	}
}

func TestASecretAndItsSealedSecretAreTheSameSecret(t *testing.T) {
	startCluster(t)
	namespace := "apiVersion: clowder/v1\nkind: Namespace\nmetadata: {name: shop}\n---\n"
	clear := writeFile(t, "clear.yaml", namespace+
		"apiVersion: clowder/v1\nkind: Secret\nmetadata: {namespace: shop, name: db}\nspec: {stringData: {password: s3cret}}\n")
	if out, err := run(t, "apply", "-f", clear); err != nil || !strings.Contains(out, "Secret shop/db: applied") {
		t.Fatalf("apply the Secret: %v\n%s", err, out)
	}
	sealed, err := run(t, "secret", "seal", "shop/db", "--from-literal", "password=s3cret")
	if err != nil {
		t.Fatal(err)
	}
	sealedFile := writeFile(t, "sealed.yaml", sealed)
	// the same value, sealed: nothing changes, as the digest is the same
	for _, command := range [][]string{{"apply", "-f", sealedFile}, {"diff", "-f", sealedFile}} {
		if out, err := run(t, command...); err != nil || strings.Contains(out, "applied") {
			t.Errorf("%v: %v\n%s", command, err, out)
		}
	}
	if out, err := run(t, "get", "secrets", "-n", "shop"); err != nil || strings.Count(out, "db") != 1 {
		t.Errorf("get secrets: %v\n%s", err, out)
	}
	if out, err := run(t, "delete", "-f", sealedFile); err != nil || out != "SealedSecret shop/db: deleted\n" {
		t.Errorf("delete -f: %v\n%s", err, out)
	}
	if _, err := run(t, "get", "secret", "shop/db"); err == nil {
		t.Error("the Secret is still there")
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

func TestSealedValuesAreReplacedOnTheirLines(t *testing.T) {
	content := "# kept\r\napiVersion: clowder/v1\r\nkind: SealedSecret\r\nmetadata: {name: db}\r\n" +
		"spec:\r\n  encryptedData:\r\n    a: \"YWJj\" # quoted, kept\r\n    b: ZGVm\r\n"
	values, err := fileValues("db.yaml", []byte(content))
	if err != nil || len(values) != 2 {
		t.Fatalf("values %+v, %v", values, err)
	}
	labels := []string{values[0].label(), values[1].label()}
	if !slices.Equal(labels, []string{"default/db[a]", "default/db[b]"}) {
		t.Errorf("labels %v", labels)
	}
	replaced, err := replaceValues([]byte(content), values, []string{"MTIz", "NDU2"})
	want := strings.NewReplacer("YWJj", "MTIz", "ZGVm", "NDU2").Replace(content)
	if err != nil || string(replaced) != want {
		t.Errorf("replaced %q, %v", replaced, err)
	}
	folded := "apiVersion: clowder/v1\nkind: SealedSecret\nmetadata: {name: db}\nspec:\n  encryptedData:\n    a: YW\n      Jj\n"
	if values, err = fileValues("db.yaml", []byte(folded)); err != nil {
		t.Fatal(err)
	}
	if _, err := replaceValues([]byte(folded), values, []string{"MTIz"}); err == nil ||
		!strings.Contains(err.Error(), "on one line") {
		t.Errorf("a value over two lines: %v", err)
	}
}
