package agent

import (
	"net/netip"
	"slices"
	"testing"
)

func TestTheAPIListensOnEachAddressOnce(t *testing.T) {
	underlay, node, public := netip.MustParseAddr("10.0.1.2"), netip.MustParseAddr("10.77.0.1"),
		netip.MustParseAddr("198.51.100.11")
	for name, test := range map[string]struct {
		host Host
		want []string
	}{
		"private network": {
			Host{Underlay: underlay, NodeAddress: node, PublicAddress: public, APIPort: 7443},
			[]string{"10.0.1.2:7443", "10.77.0.1:7443", "198.51.100.11:7443"},
		},
		"public network": {
			Host{Underlay: public, NodeAddress: node, PublicAddress: public, APIPort: 7443},
			[]string{"198.51.100.11:7443", "10.77.0.1:7443"},
		},
		"no public address": {
			Host{Underlay: underlay, NodeAddress: node, APIPort: 7443},
			[]string{"10.0.1.2:7443", "10.77.0.1:7443"},
		},
	} {
		if got := test.host.apiAddresses(); !slices.Equal(got, test.want) {
			t.Errorf("%s: %v, want %v", name, got, test.want)
		}
	}
}
