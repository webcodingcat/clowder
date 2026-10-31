package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/webcodingcat/clowder/internal/clusterdir"
	"github.com/webcodingcat/clowder/internal/resource"
	"github.com/webcodingcat/clowder/internal/sopsage"
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
	dir := startCluster(t)
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
		{"delete", "volume"},
		{"get", "pods"},
	} {
		if _, err := run(t, append([]string{"--cluster", dir}, args...)...); ExitCode(err) != 2 {
			t.Errorf("%v: exit code %d (%v), want 2", args, ExitCode(err), err)
		}
	}
	if _, err := run(t, "--cluster", dir, "get", "namespace", "nowhere"); ExitCode(err) != 1 {
		t.Errorf("get of a missing namespace: exit code %d (%v), want 1", ExitCode(err), err)
	}
	if out, err := run(t, "--cluster", dir, "stack"); err != nil || !strings.Contains(out, "Available Commands") {
		t.Errorf("a command of commands shows its help: %v\n%s", err, out)
	}
}

// writeFiles writes the files of a directory of resources: plain YAML, a
// secret in a SOPS file of its own, and files apply skips.
func writeFiles(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// the volume before its namespace: apply sorts
	write("shop.yaml", `
apiVersion: clowder/v1
kind: Volume
metadata: {namespace: shop, name: data}
spec: {class: fast, size: 1024M, replicas: 1}
---
apiVersion: clowder/v1
kind: Namespace
metadata: {name: shop}
`)
	write("compose.yaml", "services: {web: {image: nginx}}\n")
	write(".hidden.yaml", "apiVersion: clowder/v1\nkind: Namespace\nmetadata: {name: hidden}\n")
	write("notes.txt", "not YAML\n")
	identity, err := age.ParseX25519Identity(os.Getenv("SOPS_AGE_KEY"))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := sopsage.Encrypt([]byte("apiVersion: clowder/v1\nkind: Secret\nmetadata: {namespace: shop, name: token}\n"+
		"spec: {value: s3cret}\n"), []string{identity.Recipient().String()}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	write("token.sops.yaml", string(sealed))
	// another SOPS file, such as the cluster's secrets, sealed for someone
	// else: skipped, not opened
	other, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	stackSecrets, err := sopsage.Encrypt([]byte("db-password: x\n"), []string{other.Recipient().String()}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	write("secrets.sops.yaml", string(stackSecrets))
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
	cluster := startCluster(t)
	files := writeFiles(t)
	out, err := runWithInput(t, "", "--cluster", cluster, "apply", "-f", files)
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, out)
	}
	for _, want := range []string{
		"Namespace shop: applied", "Secret shop/token: applied", "Volume shop/data: applied",
		"skipped " + filepath.Join(files, "compose.yaml") + ": no resource of clowder's",
		"skipped " + filepath.Join(files, ".hidden.yaml") + ": hidden",
		"skipped " + filepath.Join(files, "notes.txt") + ": not a YAML file",
		"skipped " + filepath.Join(files, "secrets.sops.yaml") + ": no resource of clowder's",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
	if strings.Index(out, "Namespace shop") > strings.Index(out, "Volume shop/data") {
		t.Errorf("the volume went before its namespace:\n%s", out)
	}

	out, err = run(t, "--cluster", cluster, "get", "volumes", "-n", "shop")
	if err != nil || !strings.Contains(out, "data") || !strings.Contains(out, "fast") || !strings.Contains(out, "1G") ||
		!strings.Contains(out, "pending") {
		t.Errorf("get volumes: %v\n%s", err, out)
	}
	out, err = run(t, "--cluster", cluster, "get", "secrets", "-A")
	if err != nil || !strings.Contains(out, "token") || strings.Contains(out, "s3cret") {
		t.Errorf("get secrets: %v\n%s", err, out)
	}

	// what get -o yaml shows applies again as it is, from stdin
	shown, err := run(t, "--cluster", cluster, "get", "volume", "shop/data", "-o", "yaml")
	if err != nil || !strings.Contains(shown, "size: 1G") {
		t.Fatalf("get -o yaml: %v\n%s", err, shown)
	}
	out, err = runWithInput(t, shown, "--cluster", cluster, "apply", "-f", "-")
	if err != nil || !strings.Contains(out, "Volume shop/data: unchanged") {
		t.Errorf("the shown volume applied again: %v\n%s", err, out)
	}

	out, err = runWithInput(t, "apiVersion: clowder/v1\nkind: Namespace\nmetadata: {name: blog}\n",
		"--cluster", cluster, "apply", "--dry-run", "-f", "-")
	if err != nil || !strings.Contains(out, "Namespace blog: applied (dry run)") {
		t.Errorf("dry run: %v\n%s", err, out)
	}
	if _, err := run(t, "--cluster", cluster, "get", "namespace", "blog"); err == nil {
		t.Error("the dry run made the namespace")
	}

	out, err = runWithInput(t, "", "--cluster", cluster, "delete", "-f", files)
	if err != nil || !strings.Contains(out, "Namespace shop: deleted") ||
		strings.Index(out, "Volume shop/data: deleted") > strings.Index(out, "Namespace shop: deleted") {
		t.Errorf("delete -f: %v\n%s", err, out)
	}
}

func TestAStackIsListedAndAskedForInItsNamespace(t *testing.T) {
	cluster := startCluster(t)
	out, err := runWithInput(t, `
apiVersion: clowder/v1
kind: Stack
metadata: {namespace: shop, name: web}
spec: {services: {app: {image: example/app}}}
---
apiVersion: clowder/v1
kind: Namespace
metadata: {name: shop}
`, "--cluster", cluster, "apply", "-f", "-")
	if err != nil || strings.Index(out, "Namespace shop") > strings.Index(out, "Stack shop/web: applied") {
		t.Fatalf("apply: %v\n%s", err, out)
	}
	out, err = run(t, "--cluster", cluster, "get", "stacks", "-n", "shop")
	if err != nil || !strings.Contains(out, "web") || !strings.Contains(out, "pending") ||
		!strings.Contains(out, "500 (default)") {
		t.Errorf("get stacks: %v\n%s", err, out)
	}
	for _, args := range [][]string{{"stack", "ps", "-n", "shop", "web"}, {"stack", "logs", "shop/web"}} {
		_, err := run(t, append([]string{"--cluster", cluster}, args...)...)
		if err == nil || !strings.Contains(err.Error(), "stack shop/web is not placed on a host yet") {
			t.Errorf("%v: %v", args, err)
		}
	}
	if _, err := run(t, "--cluster", cluster, "stack", "ps", "web"); err == nil || !strings.Contains(err.Error(), "no stack default/web") {
		t.Errorf("stack ps in default: %v", err)
	}

	out, err = run(t, "--cluster", cluster, "stack", "restart", "-n", "shop", "web", "app")
	if err != nil || out != "Stack shop/web: restarting\n" {
		t.Errorf("stack restart: %v\n%s", err, out)
	}
	if shown, err := run(t, "--cluster", cluster, "get", "stack", "shop/web", "-o", "yaml"); err != nil ||
		!strings.Contains(shown, "restarted:") || !strings.Contains(shown, "generation: 1") {
		t.Errorf("a restarted stack: %v\n%s", err, shown)
	}
	if _, err := run(t, "--cluster", cluster, "stack", "restart", "shop/web", "mail"); err == nil ||
		!strings.Contains(err.Error(), "stack shop/web has no service mail") {
		t.Errorf("restart of no service: %v", err)
	}
	if _, err := run(t, "--cluster", cluster, "stack", "restart"); ExitCode(err) != 2 {
		t.Errorf("restart without a stack: %v", err)
	}
}

func TestASecretInClearInAFileIsWarnedOf(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token.yaml")
	secret := "apiVersion: clowder/v1\nkind: Secret\nmetadata: {name: token}\nspec: {value: s3cret}\n"
	if err := os.WriteFile(path, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	var report bytes.Buffer
	if _, err := readResources(dir, nil, &report); err != nil ||
		!strings.Contains(report.String(), "warning: "+path+" holds Secret token in clear") {
		t.Errorf("a directory: %v\n%s", err, report.String())
	}
	report.Reset()
	if _, err := readResources("-", strings.NewReader(secret), &report); err != nil || report.Len() != 0 {
		t.Errorf("stdin: %v\n%s", err, report.String())
	}
}

func TestARefusalNamesItsResourceAndStopsTheRest(t *testing.T) {
	cluster := startCluster(t)
	out, err := runWithInput(t, `
apiVersion: clowder/v1
kind: Volume
metadata: {name: data}
spec: {class: slow, size: 1G, replicas: 1}
---
apiVersion: clowder/v1
kind: Volume
metadata: {name: logs}
spec: {class: fast, size: 1G, replicas: 1}
`, "--cluster", cluster, "apply", "-f", "-")
	if ExitCode(err) != 1 || !strings.Contains(out, `Volume default/data: refused`) ||
		!strings.Contains(out, "Volume default/logs: not-tried") {
		t.Errorf("exit code %d (%v)\n%s", ExitCode(err), err, out)
	}
}

func TestFilesOfTheWrongSortAreRefused(t *testing.T) {
	cluster := startCluster(t)
	if _, err := run(t, "--cluster", cluster, "apply", "-f", cluster); err == nil ||
		!strings.Contains(err.Error(), "it is a cluster directory") {
		t.Errorf("a cluster directory: %v", err)
	}
	if _, err := runWithInput(t, "services: {}\n", "--cluster", cluster, "apply", "-f", "-"); err == nil ||
		!strings.Contains(err.Error(), "holds no resource of clowder's") {
		t.Errorf("stdin of other documents: %v", err)
	}
	identity, err := age.ParseX25519Identity(os.Getenv("SOPS_AGE_KEY"))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := sopsage.Encrypt([]byte("apiVersion: clowder/v1\nkind: Namespace\nmetadata: {name: shop}\n"),
		[]string{identity.Recipient().String()}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runWithInput(t, string(sealed), "--cluster", cluster, "apply", "-f", "-"); err == nil ||
		!strings.Contains(err.Error(), "a SOPS file holds exactly one Secret") {
		t.Errorf("a SOPS file of a namespace: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cluster, clusterdir.ConfigFile)); err != nil {
		t.Fatal(err)
	}
}
