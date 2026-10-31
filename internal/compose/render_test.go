package compose

import (
	"bytes"
	"context"
	"flag"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/webcodingcat/clowder/internal/names"
)

var update = flag.Bool("update", false, "write the golden files anew")

// loadShop loads the example stack in testdata/shop.
func loadShop(t *testing.T) *Stack {
	t.Helper()
	stack, err := Load(context.Background(), LoadOptions{
		Name: "shop", WorkingDir: "testdata/shop", Files: []string{"testdata/shop/compose.yaml"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return stack
}

// shopTarget is a host where every volume of the shop is mounted.
func shopTarget() Target {
	mounted := func(volume string, generation int64) MountedVolume {
		name := names.VolumeName(names.VolumeID("default", "shop", volume), generation)
		return MountedVolume{Dir: "/var/lib/clowder/volumes/" + name, Name: name}
	}
	return Target{
		Namespace:   "default",
		FloatingIP:  netip.MustParseAddr("203.0.113.11"),
		NodeAddress: netip.MustParseAddr("10.77.0.3"),
		Resolver:    netip.MustParseAddr("169.254.77.53"),
		Volumes:     map[string]MountedVolume{"pgdata": mounted("pgdata", 1), "cachedata": mounted("cachedata", 2)},
		SecretsDir:  "/run/clowder-secrets/default_shop/files",
		// only db asks for a secret file; app's secret is a variable
		SecretDigests: map[string]string{"db": "digest-of-db-password"},
	}
}

// matchGolden compares a rendered file with its golden file in
// testdata/<dir>; -update writes it anew.
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

func TestRenderedShopMatchesTheGoldenFile(t *testing.T) {
	rendered, err := Render(loadShop(t), shopTarget())
	if err != nil {
		t.Fatal(err)
	}
	matchGolden(t, "shop", "rendered.golden.yaml", rendered)
}

func TestTheSecretVariablesMatchTheGoldenFile(t *testing.T) {
	stack := loadShop(t)
	if _, err := RenderSecretEnv(stack, map[string]string{}); err == nil {
		t.Error("rendered without the value")
	}
	rendered, err := RenderSecretEnv(stack, map[string]string{"db-password": "pa$$word\nline two", "unused": "x"})
	if err != nil {
		t.Fatal(err)
	}
	matchGolden(t, "shop", "secrets.golden.yaml", rendered)
}

func TestRenderNeedsACompleteTarget(t *testing.T) {
	unmounted := shopTarget()
	unmounted.Volumes = map[string]MountedVolume{}
	noFloatingIP := shopTarget()
	noFloatingIP.FloatingIP = netip.Addr{}
	noDigest := shopTarget()
	noDigest.SecretDigests = nil
	noNamespace := shopTarget()
	noNamespace.Namespace = ""
	for want, target := range map[string]Target{
		"not mounted": unmounted, "no floating IP": noFloatingIP, "no digest of the secrets of service db": noDigest,
		`namespace "" is not valid`: noNamespace,
	} {
		if _, err := Render(loadShop(t), target); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want an error with %q, got %v", want, err)
		}
	}
}

func TestTwoNamespacesRenderApart(t *testing.T) {
	other := shopTarget()
	other.Namespace, other.SecretsDir = "other", "/run/clowder-secrets/other_shop/files"
	rendered, err := Render(loadShop(t), other)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"name: other_shop", "clowder.stack: other_shop", "name: other_shop_default"} {
		if !strings.Contains(string(rendered), want) {
			t.Errorf("the stack of namespace other lacks %q:\n%s", want, rendered)
		}
	}
	mine, err := Render(loadShop(t), shopTarget())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rendered), "default_shop") || bytes.Equal(rendered, mine) {
		t.Error("two namespaces render the same names")
	}
}
