package compose

import (
	"bytes"
	"flag"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/stackspec"
)

var update = flag.Bool("update", false, "write the golden files anew")

// loadSpec reads testdata/spec/stack.yaml as the API stores it: checked
// and normalized.
func loadSpec(t *testing.T) stackspec.Spec {
	t.Helper()
	content, err := os.ReadFile("testdata/spec/stack.yaml")
	if err != nil {
		t.Fatal(err)
	}
	spec, err := stackspec.Decode(content)
	if err != nil {
		t.Fatal(err)
	}
	if problems := stackspec.Check(spec); len(problems) > 0 {
		t.Fatal(problems)
	}
	return stackspec.Normalize(spec)
}

// specTarget is a host where the volumes of the namespace shop that web
// names are mounted.
func specTarget() Target {
	mounted := func(volume string) MountedVolume {
		name := names.VolumeName(names.ID("shop", volume), 1)
		return MountedVolume{Dir: "/var/lib/clowder/volumes/" + name, Name: name}
	}
	return Target{
		Namespace:    "shop",
		FloatingIP:   netip.MustParseAddr("203.0.113.11"),
		NodeAddress:  netip.MustParseAddr("10.77.0.3"),
		Resolver:     netip.MustParseAddr("169.254.77.53"),
		Volumes:      map[string]MountedVolume{"data": mounted("data"), "uploads": mounted("uploads")},
		SecretsDir:   "/run/clowder-secrets/shop_web/files",
		SecretLabels: map[string]string{"app": "digest-of-api-key"},
	}
}

// matchGolden compares a rendered file with testdata/<dir>/<name>; with
// -update it writes the file first.
func matchGolden(t *testing.T, dir, name string, rendered []byte) {
	t.Helper()
	golden := filepath.Join("testdata", dir, name)
	if *update {
		if err := os.WriteFile(golden, rendered, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (run the test with -update to write it)", err)
	}
	if !bytes.Equal(rendered, want) {
		t.Errorf("rendered file differs from %s; run with -update and review the diff:\n%s", golden, rendered)
	}
}

func TestARenderedSpecMatchesTheGoldenFile(t *testing.T) {
	rendered, err := Render("web", loadSpec(t), specTarget())
	if err != nil {
		t.Fatal(err)
	}
	matchGolden(t, "spec", "rendered.golden.yaml", rendered)
	for _, want := range []string{
		"container_name: shop_web_app", "name: shop_web_net", "condition: service_healthy",
		"condition: service_started", "restart: \"no\"", "Hello $$USER",
	} {
		if !strings.Contains(string(rendered), want) {
			t.Errorf("the rendered file lacks %q", want)
		}
	}
}

func TestTheSecretVariablesMatchTheGoldenFile(t *testing.T) {
	spec := loadSpec(t)
	if _, err := RenderSecretEnv("web", spec, map[string]string{}); err == nil {
		t.Error("rendered without the value")
	}
	rendered, err := RenderSecretEnv("web", spec, map[string]string{"db-password": "pa$$word", "api-key": "k"})
	if err != nil {
		t.Fatal(err)
	}
	matchGolden(t, "spec", "secrets.golden.yaml", rendered)
}

func TestRenderNeedsACompleteTarget(t *testing.T) {
	unmounted := specTarget()
	unmounted.Volumes = map[string]MountedVolume{"data": unmounted.Volumes["data"]}
	noResolver := specTarget()
	noResolver.Resolver = netip.Addr{}
	noLabel := specTarget()
	noLabel.SecretLabels = nil
	badNamespace := specTarget()
	badNamespace.Namespace = "clowder-x"
	for want, target := range map[string]Target{
		"volume uploads is not mounted": unmounted, "no resolver": noResolver,
		"no label for the secrets of service app": noLabel, `namespace "clowder-x" is not valid`: badNamespace,
	} {
		if _, err := Render("web", loadSpec(t), target); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want an error with %q, got %v", want, err)
		}
	}
}

func TestEveryDollarInAValueIsMeantAsItStands(t *testing.T) {
	escaped, err := escapeDollars([]byte("a$b: x$y\nlist: [$1]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if string(escaped) != "a$b: x$$y\nlist: [$$1]\n" {
		t.Errorf("escaped %q", escaped)
	}
}
