package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/webcodingcat/clowder/internal/resource"
)

func TestKindsAndNamespacesAreNamedAsWithKubectl(t *testing.T) {
	tests := []struct {
		kind, name string
		flags      selectFlags
		want       selection
		wrong      string // a usage error with this
	}{
		{"volumes", "", selectFlags{}, selection{kind: resource.KindVolume, namespace: "default"}, ""},
		{"volume", "data", selectFlags{namespace: "shop"}, selection{kind: resource.KindVolume, namespace: "shop", name: "data"}, ""},
		{"volume", "shop/data", selectFlags{}, selection{kind: resource.KindVolume, namespace: "shop", name: "data"}, ""},
		{"volume", "shop/data", selectFlags{namespace: "shop"}, selection{kind: resource.KindVolume, namespace: "shop", name: "data"}, ""},
		{"secrets", "", selectFlags{all: true}, selection{kind: resource.KindSecret, all: true}, ""},
		{"namespaces", "", selectFlags{}, selection{kind: resource.KindNamespace}, ""},
		{"namespace", "shop", selectFlags{}, selection{kind: resource.KindNamespace, name: "shop"}, ""},
		{"clusterconfiguration", "cluster", selectFlags{}, selection{kind: resource.KindClusterConfiguration, name: "cluster"}, ""},
		{"volume", "shop/data", selectFlags{namespace: "other"}, selection{}, "names namespace shop, but -n other"},
		{"volume", "data", selectFlags{all: true}, selection{}, "takes no name"},
		{"volume", "", selectFlags{namespace: "shop", all: true}, selection{}, "not both"},
		{"namespace", "shop", selectFlags{namespace: "shop"}, selection{}, "lives in no namespace"},
		{"namespaces", "", selectFlags{all: true}, selection{}, "lives in no namespace"},
		{"namespace", "a/b", selectFlags{}, selection{}, "lives in no namespace"},
		{"pods", "", selectFlags{}, selection{}, `no kind of resource "pods"`},
	}
	for _, test := range tests {
		got, err := test.flags.resolve(test.kind, test.name)
		var wrongCall usageError
		switch {
		case test.wrong == "" && (err != nil || got != test.want):
			t.Errorf("%s %q %+v: %+v, %v; want %+v", test.kind, test.name, test.flags, got, err, test.want)
		case test.wrong != "" && (!errors.As(err, &wrongCall) || !strings.Contains(err.Error(), test.wrong)):
			t.Errorf("%s %q %+v: want a usage error with %q, got %v", test.kind, test.name, test.flags, test.wrong, err)
		}
	}
}

func TestAWrongCallEndsWithTwoAndARefusalWithOne(t *testing.T) {
	startCluster(t)
	for _, args := range [][]string{
		{"nonsense"},
		{"stack", "bogus"},
		{"delete", "-f", "x.yaml", "-n", "shop"},
		{"get", "volume", "shop/"},
		{"get", "volume", "/data"},
		{"get"},
		{"get", "volumes", "--bogus"},
		{"get", "volumes", "-o", "xml"},
		{"apply"},
		{"diff"},
		{"delete", "volume"},
		{"get", "pods"},
		{"apply", "-f", "x.yaml", "--cluster", "prod"}, // the cluster directory is gone
	} {
		if _, err := run(t, args...); ExitCode(err) != 2 {
			t.Errorf("%v: exit code %d (%v), want 2", args, ExitCode(err), err)
		}
	}
	if _, err := run(t, "get", "namespace", "nowhere"); ExitCode(err) != 1 {
		t.Errorf("get of a missing namespace: exit code %d (%v), want 1", ExitCode(err), err)
	}
	if out, err := run(t, "stack"); err != nil || !strings.Contains(out, "Available Commands") {
		t.Errorf("a command of commands shows its help: %v\n%s", err, out)
	}
}

// writeFiles writes the files of a directory of resources: plain YAML, a
// SealedSecret as secret seal writes it for the cluster, and files apply
// skips.
func writeFiles(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("shop.yaml", `
apiVersion: clowder/v1
kind: Namespace
metadata: {name: shop}
---
apiVersion: clowder/v1
kind: Volume
metadata: {namespace: shop, name: data}
spec: {class: fast, size: 1024M, replicas: 1}
`)
	write("compose.yaml", "services: {web: {image: nginx}}\n")
	write(".hidden.yaml", "apiVersion: clowder/v1\nkind: Namespace\nmetadata: {name: hidden}\n")
	write("notes.txt", "not YAML\n")
	token, err := runWithInput(t, "s3cret\n", "secret", "seal", "shop/token", "--from-file", "value=-")
	if err != nil {
		t.Fatalf("secret seal: %v\n%s", err, token)
	}
	if strings.Contains(token, "s3cret") || !strings.Contains(token, "metadata: {namespace: shop, name: token}") {
		t.Fatalf("secret seal wrote:\n%s", token)
	}
	write("token.yaml", token)
	return dir
}

// runWithInput runs the CLI with stdin.
func runWithInput(t *testing.T, input string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	root := NewRoot()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetIn(strings.NewReader(input))
	root.SetArgs(args)
	err := root.ExecuteContext(context.Background())
	return out.String(), err
}

func TestResourcesApplyFromADirectoryReadBackAndGo(t *testing.T) {
	startCluster(t)
	files := writeFiles(t)
	out, err := runWithInput(t, "", "apply", "-f", files)
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, out)
	}
	for _, want := range []string{
		"Namespace shop: applied", "Secret shop/token: applied", "Volume shop/data: applied",
		"skipped " + filepath.Join(files, "compose.yaml") + ": no resource of clowder's",
		"skipped " + filepath.Join(files, ".hidden.yaml") + ": hidden",
		"skipped " + filepath.Join(files, "notes.txt") + ": not a YAML file",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}

	out, err = run(t, "get", "volumes", "-n", "shop")
	if err != nil || !strings.Contains(out, "data") || !strings.Contains(out, "fast") || !strings.Contains(out, "1G") ||
		!strings.Contains(out, "pending") {
		t.Errorf("get volumes: %v\n%s", err, out)
	}
	out, err = run(t, "get", "secrets", "-A")
	if err != nil || !strings.Contains(out, "token") || !strings.Contains(out, "value") || strings.Contains(out, "s3cret") {
		t.Errorf("get secrets: %v\n%s", err, out)
	}

	// what get -o yaml shows applies again as it is, from stdin
	shown, err := run(t, "get", "volume", "shop/data", "-o", "yaml")
	if err != nil || !strings.Contains(shown, "size: 1G") {
		t.Fatalf("get -o yaml: %v\n%s", err, shown)
	}
	out, err = runWithInput(t, shown, "apply", "-f", "-")
	if err != nil || !strings.Contains(out, "Volume shop/data: unchanged") {
		t.Errorf("the shown volume applied again: %v\n%s", err, out)
	}

	out, err = runWithInput(t, "apiVersion: clowder/v1\nkind: Namespace\nmetadata: {name: blog}\n",
		"apply", "--dry-run", "-f", "-")
	if err != nil || !strings.Contains(out, "Namespace blog: applied (dry run)") {
		t.Errorf("dry run: %v\n%s", err, out)
	}
	if _, err := run(t, "get", "namespace", "blog"); err == nil {
		t.Error("the dry run made the namespace")
	}

	// a volume goes only with --with-data, and without it nothing goes
	if _, err := runWithInput(t, "", "delete", "-f", files); err == nil ||
		!strings.Contains(err.Error(), "deleting volume shop/data deletes its data; confirm with --with-data") {
		t.Errorf("delete -f without --with-data: %v", err)
	}
	if _, err := run(t, "get", "secret", "shop/token"); err != nil {
		t.Errorf("a refused delete took the secret: %v", err)
	}
	// from the bottom up: the Secret of the last file first, the namespace last
	out, err = runWithInput(t, "", "delete", "--with-data", "-f", files)
	want := "Secret shop/token: deleted\nVolume shop/data: deleted\nNamespace shop: deleted\n"
	if err != nil || !strings.HasSuffix(out, want) {
		t.Errorf("delete -f: %v\n%s\nwant it to end with\n%s", err, out, want)
	}
}

func TestApplyKeepsTheOrderOfTheFiles(t *testing.T) {
	startCluster(t)
	// the stack above its namespace: apply does not sort, so the stack has
	// no namespace yet, and nothing goes
	out, err := runWithInput(t, `
apiVersion: clowder/v1
kind: Stack
metadata: {namespace: shop, name: web}
spec: {services: {app: {image: example/app}}}
---
apiVersion: clowder/v1
kind: Namespace
metadata: {name: shop}
`, "apply", "-f", "-")
	if ExitCode(err) != 1 || !strings.HasPrefix(out, "Stack shop/web: refused") ||
		!strings.Contains(out, "Namespace shop: not-applied") {
		t.Errorf("exit code %d (%v)\n%s", ExitCode(err), err, out)
	}
	if _, err := run(t, "get", "namespace", "shop"); err == nil {
		t.Error("the namespace went in without the stack")
	}
}

func TestAStackIsListedAndAskedForInItsNamespace(t *testing.T) {
	startCluster(t)
	out, err := runWithInput(t, `
apiVersion: clowder/v1
kind: Namespace
metadata: {name: shop}
---
apiVersion: clowder/v1
kind: Stack
metadata: {namespace: shop, name: web}
spec: {services: {app: {image: example/app}}}
`, "apply", "-f", "-")
	if err != nil || out != "Namespace shop: applied\nStack shop/web: applied\n" {
		t.Fatalf("apply: %v\n%s", err, out)
	}
	out, err = run(t, "get", "stacks", "-n", "shop")
	if err != nil || !strings.Contains(out, "web") || !strings.Contains(out, "pending") ||
		!strings.Contains(out, "500 (default)") {
		t.Errorf("get stacks: %v\n%s", err, out)
	}
	for _, args := range [][]string{{"stack", "ps", "-n", "shop", "web"}, {"stack", "logs", "shop/web"}} {
		_, err := run(t, args...)
		if err == nil || !strings.Contains(err.Error(), "stack shop/web is not placed on a host yet") {
			t.Errorf("%v: %v", args, err)
		}
	}
	if _, err := run(t, "stack", "ps", "web"); err == nil || !strings.Contains(err.Error(), "no stack default/web") {
		t.Errorf("stack ps in default: %v", err)
	}

	out, err = run(t, "stack", "restart", "-n", "shop", "web", "app")
	if err != nil || out != "Stack shop/web: restarting\n" {
		t.Errorf("stack restart: %v\n%s", err, out)
	}
	if shown, err := run(t, "get", "stack", "shop/web", "-o", "yaml"); err != nil ||
		!strings.Contains(shown, "restarted:") || !strings.Contains(shown, "generation: 1") {
		t.Errorf("a restarted stack: %v\n%s", err, shown)
	}
	if _, err := run(t, "stack", "restart", "shop/web", "mail"); err == nil ||
		!strings.Contains(err.Error(), "stack shop/web has no service mail") {
		t.Errorf("restart of no service: %v", err)
	}
	if _, err := run(t, "stack", "restart"); ExitCode(err) != 2 {
		t.Errorf("restart without a stack: %v", err)
	}
}

func TestAValueSealedForAnotherSecretIsRefused(t *testing.T) {
	startCluster(t)
	token, err := run(t, "secret", "seal", "shop/token", "--from-literal", "value=s3cret")
	if err != nil {
		t.Fatal(err)
	}
	// the same value, renamed: it stays bound to shop/token
	renamed := strings.Replace(token, "name: token", "name: copied", 1)
	out, err := runWithInput(t, "apiVersion: clowder/v1\nkind: Namespace\nmetadata: {name: shop}\n---\n"+renamed,
		"apply", "-f", "-")
	if ExitCode(err) != 1 || !strings.Contains(out, "the value is sealed for shop/token, not for shop/copied") {
		t.Errorf("exit code %d (%v)\n%s", ExitCode(err), err, out)
	}
}

func TestARefusalNamesItsResourceAndNoneGoes(t *testing.T) {
	startCluster(t)
	out, err := runWithInput(t, `
apiVersion: clowder/v1
kind: Volume
metadata: {name: logs}
spec: {class: fast, size: 1G, replicas: 1}
---
apiVersion: clowder/v1
kind: Volume
metadata: {name: data}
spec: {class: slow, size: 1G, replicas: 1}
`, "apply", "-f", "-")
	if ExitCode(err) != 1 || !strings.Contains(out, "Volume default/logs: not-applied") ||
		!strings.Contains(out, `Volume default/data: refused`) {
		t.Errorf("exit code %d (%v)\n%s", ExitCode(err), err, out)
	}
	if _, err := run(t, "get", "volume", "logs"); err == nil {
		t.Error("the volume above the refused one went in")
	}
}

func TestFilesWithoutResourcesAreRefused(t *testing.T) {
	startCluster(t)
	if _, err := runWithInput(t, "services: {}\n", "apply", "-f", "-"); err == nil ||
		!strings.Contains(err.Error(), "holds no resource of clowder's") {
		t.Errorf("stdin of other documents: %v", err)
	}
}
