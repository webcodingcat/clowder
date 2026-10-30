package ingress

import (
	"flag"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "write the golden files anew")

// golden compares rendered with testdata/<name>; -update writes it anew.
func golden(t *testing.T, name string, rendered []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, rendered, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run the test with -update to write it)", err)
	}
	if string(rendered) != string(want) {
		t.Errorf("differs from %s; run with -update and review the diff:\n%s", path, rendered)
	}
}

func TestTheStaticConfigurationMatchesTheGoldenFile(t *testing.T) {
	rendered, err := RenderStatic(Static{
		FloatingIP: netip.MustParseAddr("203.0.113.10"), NodeAddress: netip.MustParseAddr("10.77.0.1"),
		Overlay: netip.MustParsePrefix("10.77.0.0/24"),
	})
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "traefik.golden.yaml", rendered)
}

func TestTheStaticConfigurationNeedsItsAddresses(t *testing.T) {
	// before the first cluster apply there is no Floating IP
	_, err := RenderStatic(Static{NodeAddress: netip.MustParseAddr("10.77.0.1"), Overlay: netip.MustParsePrefix("10.77.0.0/24")})
	if err == nil || !strings.Contains(err.Error(), "Floating IP") {
		t.Errorf("got %v", err)
	}
}

func TestTheComposeFileMatchesTheGoldenFile(t *testing.T) {
	rendered, err := RenderCompose(Mounts{
		Static: "/var/lib/clowder/traefik/static", Dynamic: "/var/lib/clowder/traefik/dynamic",
		Certificates: "/run/clowder-secrets/clowder-traefik",
	})
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "compose.golden.yaml", rendered)
}

func TestTheRoutesMatchTheGoldenFile(t *testing.T) {
	rendered, err := RenderDynamic([]Route{
		{
			Project: "default_shop", Service: "app", Host: "Shop.example.com",
			Targets: []string{"172.18.0.3:8080", "172.18.0.2:8080"},
		},
		{
			Project: "default_shop", Service: "api", Host: "shop.example.com", Path: "/api",
			Targets: []string{"172.18.0.4:9000"},
		},
		// no container may take requests: an empty list, so Traefik answers 503
		{Project: "default_blog", Service: "web", Host: "blog.example.com", Path: "/"},
	}, []CertificateFiles{{Domain: "shop.example.com", Chain: "shop.example.com-1a2b.crt", Key: "shop.example.com-1a2b.key"}})
	// blog.example.com has no certificate yet: plain HTTP
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "routes.golden.yaml", rendered)
}
