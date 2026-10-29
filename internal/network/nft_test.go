package network

import (
	"flag"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/webcodingcat/clowder/internal/state"
)

var update = flag.Bool("update", false, "write the golden files anew")

func TestTheRulesetMatchesTheGoldenFile(t *testing.T) {
	rendered := Render(Ruleset{FloatingIP: netip.MustParseAddr("203.0.113.11")})
	golden := filepath.Join("testdata", "isolation.golden.nft")
	if *update {
		if err := os.WriteFile(golden, []byte(rendered), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (run the test with -update to write it)", err)
	}
	if rendered != string(want) {
		t.Errorf("the ruleset differs from %s; run with -update and review the diff:\n%s", golden, rendered)
	}
}

func TestWithoutAFloatingIPThereIsNoHairpin(t *testing.T) {
	rendered := Render(Ruleset{})
	if want := "tcp dport { 80, 443 }"; strings.Contains(rendered, want) {
		t.Errorf("a ruleset without Floating IP lets containers to %q:\n%s", want, rendered)
	}
}

func TestNodeAddressesFollowTheHostID(t *testing.T) {
	if got := NodeAddress(0); got != netip.MustParseAddr("10.77.0.1") {
		t.Errorf("host 0: %s", got)
	}
	if last := NodeAddress(state.MaxHosts - 1); !Overlay.Contains(last) {
		t.Errorf("the last host's address %s is outside %s", last, Overlay)
	}
}
