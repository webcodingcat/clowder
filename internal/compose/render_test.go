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
	return Target{
		Generation:  3,
		FloatingIP:  netip.MustParseAddr("203.0.113.11"),
		NodeAddress: netip.MustParseAddr("10.77.0.3"),
		Resolver:    netip.MustParseAddr("169.254.77.53"),
		Volumes: map[string]MountedVolume{
			"pgdata":    {Dir: "/var/lib/clowder/volumes/shop-pgdata", Generation: 1},
			"cachedata": {Dir: "/var/lib/clowder/volumes/shop-cachedata", Generation: 2},
		},
		SecretsDir: "/run/clowder-secrets/shop/files",
	}
}

// matchGolden compares a rendered file with its golden file in
// testdata/shop; -update writes it anew.
func matchGolden(t *testing.T, name string, rendered []byte) {
	t.Helper()
	golden := filepath.Join("testdata", "shop", name)
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
	matchGolden(t, "rendered.golden.yaml", rendered)
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
	matchGolden(t, "secrets.golden.yaml", rendered)
}

func TestRenderNeedsACompleteTarget(t *testing.T) {
	unmounted := shopTarget()
	unmounted.Volumes = map[string]MountedVolume{}
	noFloatingIP := shopTarget()
	noFloatingIP.FloatingIP = netip.Addr{}
	for want, target := range map[string]Target{"not mounted": unmounted, "no floating IP": noFloatingIP} {
		if _, err := Render(loadShop(t), target); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want an error with %q, got %v", want, err)
		}
	}
}

func TestBridgeNamesFitTheKernelLimit(t *testing.T) {
	name := BridgeName("a-stack-with-a-rather-long-name", "backend")
	if len(name) > 15 {
		t.Errorf("%s has %d characters", name, len(name))
	}
	if BridgeName("shop", "default") == BridgeName("shop", "backend") {
		t.Error("networks of one stack share a bridge name")
	}
	// stable across releases: existing bridges keep their names
	if got := BridgeName("shop", "default"); got != "cbr-8d9001d32c6" {
		t.Errorf("bridge name changed: %s", got)
	}
}

func TestVolumeNamesOfDifferentStacksNeverMeet(t *testing.T) {
	if VolumeName("a-b", "c", 1) == VolumeName("a", "b-c", 1) {
		t.Error("stack a-b volume c and stack a volume b-c share a Docker volume")
	}
	if VolumeName("shop", "pgdata", 1) == VolumeName("shop", "pgdata", 2) {
		t.Error("a new generation keeps the old volume")
	}
}
