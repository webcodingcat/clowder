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

// checkGolden compares rendered with the golden file of that name in
// testdata, or writes it with -update.
func checkGolden(t *testing.T, name, rendered string) {
	t.Helper()
	golden := filepath.Join("testdata", name)
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
		t.Errorf("the rendering differs from %s; run with -update and review the diff:\n%s", golden, rendered)
	}
}

// Host 0 of three, in a private network, with public addresses.
func TestTheRulesetMatchesTheGoldenFile(t *testing.T) {
	checkGolden(t, "isolation.golden.nft", Render(Ruleset{
		FloatingIP: netip.MustParseAddr("203.0.113.11"),
		ClusterHosts: []netip.Addr{
			netip.MustParseAddr("10.0.0.2"), netip.MustParseAddr("10.0.0.3"), netip.MustParseAddr("10.0.0.4"),
			netip.MustParseAddr("203.0.113.20"), netip.MustParseAddr("203.0.113.21"),
			netip.MustParseAddr("203.0.113.22"),
		},
		PrivateNetwork: netip.MustParsePrefix("10.0.0.0/16"),
		Peers:          []state.HostID{1, 2},
	}))
}

// Before the first cluster apply: no Floating IP, no private network.
func TestABareRulesetKeepsItsBoundaries(t *testing.T) {
	rendered := Render(Ruleset{ClusterHosts: []netip.Addr{netip.MustParseAddr("198.51.100.2")}})
	if hairpin := "tcp dport { 80, 443 }"; strings.Contains(rendered, hairpin) {
		t.Errorf("a ruleset without Floating IP lets containers to %q:\n%s", hairpin, rendered)
	}
	for _, kept := range []string{
		"fib saddr . iif oif missing counter drop", "ip daddr 169.254.169.254 counter drop",
		"ip daddr @cluster_hosts counter drop", "ip daddr 10.77.0.0/24 counter drop",
	} {
		if !strings.Contains(rendered, kept) {
			t.Errorf("a ruleset without Floating IP lacks %q:\n%s", kept, rendered)
		}
	}
}

// The spoofed source goes before anything is accepted: a container must not
// slip into a connection that exists (conntrack does not look at the
// interface).
func TestSpoofedSourcesGoFirst(t *testing.T) {
	rendered := Render(Ruleset{})
	_, forward, _ := strings.Cut(rendered, "chain guard_fwd {")
	spoofed, accepted := strings.Index(forward, "fib saddr"), strings.Index(forward, " accept\n")
	if spoofed < 0 || accepted < spoofed {
		t.Errorf("guard_fwd accepts before it drops spoofed sources:\n%s", forward)
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
