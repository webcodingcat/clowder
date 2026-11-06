package agent

import (
	"crypto/ed25519"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/webcodingcat/clowder/internal/hostrole"
	"github.com/webcodingcat/clowder/internal/pki"
	"github.com/webcodingcat/clowder/internal/state"
)

func TestTheAgentStartsOnlyWithOneSetOfRoles(t *testing.T) {
	ca, err := pki.NewCA("prod", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	key, err := pki.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	certificatePEM, err := ca.IssueHost(key.Public().(ed25519.PublicKey), 2, hostrole.DefaultJoin, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := pki.ParseCertificate(certificatePEM)
	if err != nil {
		t.Fatal(err)
	}
	inState := func(roles hostrole.Set) *state.State {
		return &state.State{Hosts: map[state.HostID]*state.Host{2: {ID: 2, Roles: roles}}}
	}
	for name, test := range map[string]struct {
		file    hostrole.Set
		current *state.State
		want    string // "" for none
	}{
		"all three agree":                {hostrole.DefaultJoin, inState(hostrole.DefaultJoin), ""},
		"a state that knows no host yet": {hostrole.DefaultJoin, &state.State{}, ""},
		"host.json changed by hand":      {hostrole.DefaultInit, inState(hostrole.DefaultJoin), "certificate"},
		"another state":                  {hostrole.DefaultJoin, inState(hostrole.DefaultInit), "state"},
	} {
		host := Host{ID: 2, Roles: test.file}
		err := host.checkRoles(certificate, test.current)
		switch {
		case test.want == "" && err != nil:
			t.Errorf("%s: %v", name, err)
		case test.want != "" && (err == nil || !strings.Contains(err.Error(), test.want)):
			t.Errorf("%s: want an error about the %s, got %v", name, test.want, err)
		}
	}
}

func TestTheAPIListensOnEachAddressOnce(t *testing.T) {
	underlay, node, public := netip.MustParseAddr("10.0.1.2"), netip.MustParseAddr("10.77.0.1"),
		netip.MustParseAddr("198.51.100.11")
	for name, test := range map[string]struct {
		host Host
		want []string
	}{
		"a control-plane host": {
			Host{Roles: hostrole.DefaultInit, Underlay: underlay, NodeAddress: node, PublicAddress: public, APIPort: 7443},
			[]string{"10.0.1.2:7443", "10.77.0.1:7443", "198.51.100.11:7443"},
		},
		// only the control plane calls it, over the private network
		"a worker": {
			Host{Roles: hostrole.DefaultJoin, Underlay: underlay, NodeAddress: node, PublicAddress: public, APIPort: 7443},
			[]string{"10.0.1.2:7443", "10.77.0.1:7443"},
		},
		// as the tests run it, on one machine
		"one address twice": {
			Host{Roles: hostrole.DefaultInit, Underlay: underlay, NodeAddress: underlay, PublicAddress: public, APIPort: 7443},
			[]string{"10.0.1.2:7443", "198.51.100.11:7443"},
		},
	} {
		if got := test.host.apiAddresses(); !slices.Equal(got, test.want) {
			t.Errorf("%s: %v, want %v", name, got, test.want)
		}
	}
}
