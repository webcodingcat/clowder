package secretvalue

import (
	"flag"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "write the golden files anew")

// fakeArmor stands for armor where only its lines count.
func fakeArmor(body string) string {
	return "-----BEGIN AGE ENCRYPTED FILE-----\n" + body + "\n-----END AGE ENCRYPTED FILE-----\n"
}

func TestExtractPutsEachPathInPlaceOfItsValue(t *testing.T) {
	content, err := os.ReadFile(filepath.Join("testdata", "rewrite.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	documents, err := Extract(content)
	if err != nil {
		t.Fatal(err)
	}
	if len(documents) != 2 {
		t.Fatalf("got %d documents", len(documents))
	}
	want := map[string]string{
		"hoster.token": fakeArmor("b2xkIHRva2Vu"),
		"join_keys[0]": fakeArmor("b2xkIGpvaW4ga2V5"),
		"join_keys[1]": fakeArmor("b2xkIHNlY29uZA=="),
		`registries["münchen.example:5000"].password`: fakeArmor("b2xkIHJlZ2lzdHJ5"),
	}
	if !maps.Equal(documents[0].Secrets, want) {
		t.Errorf("got %v", documents[0].Secrets)
	}
	if !maps.Equal(documents[1].Secrets, map[string]string{"spec.value": fakeArmor("b2xkIHNlY3JldA==")}) {
		t.Errorf("got %v", documents[1].Secrets)
	}

	var config struct {
		Name   string `yaml:"name"`
		Hoster struct {
			Token string `yaml:"token"`
		} `yaml:"hoster"`
		JoinKeys   []string `yaml:"join_keys"`
		Registries map[string]struct {
			Password string `yaml:"password"`
		} `yaml:"registries"`
	}
	if err := documents[0].Node.Decode(&config); err != nil {
		t.Fatal(err)
	}
	if config.Name != "prod" || config.Hoster.Token != "hoster.token" ||
		!slices.Equal(config.JoinKeys, []string{"join_keys[0]", "join_keys[1]"}) ||
		config.Registries["münchen.example:5000"].Password != `registries["münchen.example:5000"].password` {
		t.Errorf("decoded %+v", config)
	}
}

func TestExtractRefusesValuesItCannotReplace(t *testing.T) {
	for _, test := range []struct{ name, content, want string }{
		{"quoted", "token: !secret \"abc\"\n", "write !secret |"},
		{"plain", "token: !secret abc\n", "write !secret |"},
		{"folded", "token: !secret >\n  abc\n", "write !secret |"},
		{"indentation indicator", "token: !secret |2\n    abc\n", "write !secret |"},
		{"anchored", "token: &a !secret |\n  abc\n", "write !secret |"},
		{"blank line before the armor", "token: !secret |\n\n  abc\n", "write !secret |"},
		{"in a flow list", "keys: [!secret abc]\n", "keys[0]: write !secret |"},
		{"empty", "token: !secret |\nname: x\n", "token: the value is empty"},
		{"empty at the end", "token: !secret |\n", "token: the value is empty"},
		{"on a mapping", "hoster: !secret\n  token: x\n", "hoster: !secret tags a block scalar"},
		{"on a list", "keys: !secret [a]\n", "keys: !secret tags a block scalar"},
		{"on a key", "? !secret |\n  abc\n: x\n", "tags a value, not a key"},
		{"below a complex key", "? [a, b]\n: !secret |\n  abc\n", "below a key that is not a scalar"},
		{"twice", "a: !secret |\n  abc\na: !secret |\n  def\n", "a: two values at the same place"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := Extract([]byte(test.content))
			if err == nil {
				t.Fatal("no error")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Errorf("got %v, want %q", err, test.want)
			}
		})
	}
}

func TestExtractLeavesOtherTagsAndFilesAlone(t *testing.T) {
	documents, err := Extract([]byte("a: !Secret x\nb: [1, 2]\n? [c, d]\n: e\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(documents) != 1 || len(documents[0].Secrets) != 0 {
		t.Errorf("got %+v", documents)
	}
	if documents, err := Extract(nil); err != nil || len(documents) != 0 {
		t.Errorf("an empty file gave %v, %v", documents, err)
	}
}

// replaceAll gives every value new armor that names its document and path.
func replaceAll(document int, path, armored string) (string, error) {
	if !strings.HasPrefix(armored, "-----BEGIN AGE ENCRYPTED FILE-----\n") {
		return "", fmt.Errorf("got %q", armored)
	}
	return fakeArmor(fmt.Sprintf("new %d %s\nsecond line", document, path)), nil
}

func TestRewriteReplacesOnlyTheArmor(t *testing.T) {
	content, err := os.ReadFile(filepath.Join("testdata", "rewrite.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	rewritten, err := Rewrite(content, replaceAll)
	if err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "rewrite.golden.yaml")
	if *update {
		if err := os.WriteFile(golden, rewritten, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if string(rewritten) != string(want) {
		t.Errorf("rewritten:\n%s", rewritten)
	}

	// Windows' line ends stay, and a file without a last line end stays so
	crlf := strings.TrimSuffix(strings.ReplaceAll(string(content), "\n", "\r\n"), "\r\n")
	rewritten, err = Rewrite([]byte(crlf), replaceAll)
	if err != nil {
		t.Fatal(err)
	}
	if wantCRLF := strings.TrimSuffix(strings.ReplaceAll(string(want), "\n", "\r\n"), "\r\n"); string(rewritten) != wantCRLF {
		t.Errorf("rewritten with CRLF:\n%q", rewritten)
	}
}

func TestRewriteKeepsTheValuesYAMLReads(t *testing.T) {
	content, err := os.ReadFile(filepath.Join("testdata", "rewrite.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	rewritten, err := Rewrite(content, replaceAll)
	if err != nil {
		t.Fatal(err)
	}
	documents, err := Extract(rewritten)
	if err != nil {
		t.Fatal(err)
	}
	for index, document := range documents {
		for path, armored := range document.Secrets {
			if want, _ := replaceAll(index, path, fakeArmor("")); armored != want {
				t.Errorf("%d %s: got %q", index, path, armored)
			}
		}
	}
}

func TestRewriteRefusesArmorWithAnEmptyLine(t *testing.T) {
	_, err := Rewrite([]byte("token: !secret |\n  abc\n"), func(int, string, string) (string, error) {
		return "a\n\nb\n", nil
	})
	if err == nil || !strings.Contains(err.Error(), "token: the new armor has an empty line") {
		t.Errorf("got %v", err)
	}
}

func TestEncryptedValuesComeBackThroughAFile(t *testing.T) {
	cluster, recovery := identities(t)
	token, err := Encrypt(Value{For: "clowder-system/hetzner", Bytes: []byte("hcloud token")},
		cluster.Recipient(), recovery.Recipient())
	if err != nil {
		t.Fatal(err)
	}
	file := "hoster:\n  token: " + Block(token, "    ")
	documents, err := Extract([]byte(file))
	if err != nil {
		t.Fatal(err)
	}
	opened, err := Open(documents[0].Secrets["hoster.token"], recovery)
	if err != nil {
		t.Fatal(err)
	}
	if opened.For != "clowder-system/hetzner" || string(opened.Bytes) != "hcloud token" {
		t.Errorf("got %q for %s", opened.Bytes, opened.For)
	}
	var config map[string]map[string]string
	if err := documents[0].Node.Decode(&config); err != nil || config["hoster"]["token"] != "hoster.token" {
		t.Errorf("decoded %v, %v", config, err)
	}
}
