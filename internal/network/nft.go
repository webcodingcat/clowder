package network

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/webcodingcat/clowder/internal/command"
)

// Fixed addresses of clowder's network.
var (
	// Resolver is the DNS resolver every container uses; it sits on clowder0.
	Resolver = netip.MustParseAddr("169.254.77.53")
	// Overlay holds the node addresses.
	Overlay = netip.MustParsePrefix("10.77.0.0/24")
	// metadataService is the hoster's metadata API: its user data holds the
	// bootstrap key, so no container may reach it.
	metadataService = netip.MustParseAddr("169.254.169.254")
)

// containerBridges are the interfaces containers send from: clowder's stack
// bridges, Docker's default bridge and Compose networks clowder did not make.
const containerBridges = `{ "cbr-*", "docker0", "br-*" }`

// Ruleset is what goes into the nft table inet clowder.
type Ruleset struct {
	FloatingIP netip.Addr
}

// Render writes the table inet clowder. It deletes and recreates the table
// in one transaction, so nft -f replaces it atomically and nothing else's
// rules (Docker's) are touched.
//
// What containers may reach on the host: the resolver, the local Traefik
// on the Floating IP (HTTP hairpin) and answers to their own connections;
// nothing else. What they may not reach beyond it: the metadata API, and the
// node addresses of other hosts unless clowder itself redirected them there
// (DNAT: exports and hairpin).
func Render(ruleset Ruleset) string {
	lines := []string{
		"table inet clowder",
		"delete table inet clowder",
		"table inet clowder {",
		"  chain guard_in {",
		"    type filter hook input priority filter; policy accept;",
		"    iifname " + containerBridges + " ct state established,related accept",
		fmt.Sprintf("    iifname %s ip daddr %s meta l4proto { tcp, udp } th dport 53 accept", containerBridges, Resolver),
	}
	if ruleset.FloatingIP.IsValid() {
		lines = append(lines,
			fmt.Sprintf("    iifname %s ip daddr %s tcp dport { 80, 443 } accept", containerBridges, ruleset.FloatingIP))
	}
	lines = append(lines,
		"    iifname "+containerBridges+" drop",
		"  }",
		"  chain guard_fwd {",
		"    type filter hook forward priority filter - 10; policy accept;",
		fmt.Sprintf("    iifname %s ip daddr %s drop", containerBridges, metadataService),
		fmt.Sprintf("    iifname %s ip daddr %s ct status dnat accept", containerBridges, Overlay),
		fmt.Sprintf("    iifname %s ip daddr %s drop", containerBridges, Overlay),
		"  }",
		"}",
	)
	return strings.Join(lines, "\n") + "\n"
}

// toolTimeout bounds nft and ip, which answer in milliseconds when well.
const toolTimeout = 30 * time.Second

// Apply checks the ruleset with nft and then loads it.
func Apply(ctx context.Context, ruleset string) error {
	for _, args := range [][]string{{"-c", "-f", "-"}, {"-f", "-"}} {
		if _, err := command.RunWithInput(ctx, toolTimeout, ruleset, "nft", args...); err != nil {
			return err
		}
	}
	return nil
}
