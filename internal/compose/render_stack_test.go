package compose

import (
	"net/netip"
	"os"
	"strings"
	"testing"

	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/stackspec"
)

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

// specTarget is a host where the volumes of web in the namespace shop are
// mounted.
func specTarget() Target {
	mounted := func(volume string) MountedVolume {
		name := names.VolumeName(names.VolumeID("shop", "web", volume), 1)
		return MountedVolume{Dir: "/var/lib/clowder/volumes/" + name, Name: name}
	}
	return Target{
		Namespace:     "shop",
		FloatingIP:    netip.MustParseAddr("203.0.113.11"),
		NodeAddress:   netip.MustParseAddr("10.77.0.3"),
		Resolver:      netip.MustParseAddr("169.254.77.53"),
		Volumes:       map[string]MountedVolume{"data": mounted("data"), "uploads": mounted("uploads")},
		SecretsDir:    "/run/clowder-secrets/shop_web/files",
		SecretDigests: map[string]string{"app": "digest-of-api-key"},
	}
}

func TestARenderedSpecMatchesTheGoldenFile(t *testing.T) {
	rendered, err := RenderStack("web", loadSpec(t), specTarget())
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

func TestTheSpecsSecretVariablesMatchTheGoldenFile(t *testing.T) {
	spec := loadSpec(t)
	if _, err := RenderStackSecretEnv("web", spec, map[string]string{}); err == nil {
		t.Error("rendered without the value")
	}
	rendered, err := RenderStackSecretEnv("web", spec, map[string]string{"db-password": "pa$$word", "api-key": "k"})
	if err != nil {
		t.Fatal(err)
	}
	matchGolden(t, "spec", "secrets.golden.yaml", rendered)
}

func TestRenderStackNeedsACompleteTarget(t *testing.T) {
	unmounted := specTarget()
	unmounted.Volumes = map[string]MountedVolume{"data": unmounted.Volumes["data"]}
	noResolver := specTarget()
	noResolver.Resolver = netip.Addr{}
	noDigest := specTarget()
	noDigest.SecretDigests = nil
	badNamespace := specTarget()
	badNamespace.Namespace = "clowder-x"
	for want, target := range map[string]Target{
		"volume uploads is not mounted": unmounted, "no resolver": noResolver,
		"no digest of the secrets of service app": noDigest, `namespace "clowder-x" is not valid`: badNamespace,
	} {
		if _, err := RenderStack("web", loadSpec(t), target); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want an error with %q, got %v", want, err)
		}
	}
}
