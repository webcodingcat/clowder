package compose

import (
	"bytes"
	"context"
	"flag"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
)

var update = flag.Bool("update", false, "write the golden files anew")

func TestRenderedShopMatchesTheGoldenFile(t *testing.T) {
	stack, err := Load(context.Background(), LoadOptions{
		Name:       "shop",
		WorkingDir: "testdata/shop",
		Files:      []string{"testdata/shop/compose.yaml"},
	})
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := Render(stack, Target{
		Generation:  3,
		FloatingIP:  netip.MustParseAddr("203.0.113.11"),
		NodeAddress: netip.MustParseAddr("10.77.0.3"),
		Resolver:    netip.MustParseAddr("169.254.77.53"),
		Volumes: map[string]MountedVolume{
			"pgdata":    {Dir: "/var/lib/clowder/volumes/shop-pgdata", Generation: 1},
			"cachedata": {Dir: "/var/lib/clowder/volumes/shop-cachedata", Generation: 2},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	golden := filepath.Join("testdata", "shop", "rendered.golden.yaml")
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
		t.Errorf("rendered stack differs from %s; run with -update and review the diff:\n%s",
			golden, rendered)
	}
}

func TestRenderNeedsEveryVolumeMounted(t *testing.T) {
	stack, err := Load(context.Background(), LoadOptions{
		Name: "shop", WorkingDir: "testdata/shop", Files: []string{"testdata/shop/compose.yaml"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Render(stack, Target{Volumes: map[string]MountedVolume{}}); err == nil {
		t.Error("no error for unmounted volumes")
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
