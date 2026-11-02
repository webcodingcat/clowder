package agent

import (
	"net/netip"
	"reflect"
	"testing"

	"github.com/webcodingcat/clowder/internal/agent/agenttest"
	"github.com/webcodingcat/clowder/internal/network"
	"github.com/webcodingcat/clowder/internal/state"
)

func TestTheWantedNetworkHasEveryOtherHostAsAPeer(t *testing.T) {
	cluster := agenttest.NewCluster(t)
	cluster.State.Config.Spec.PrivateNetwork = "10.0.0.0/16"
	cluster.State.Hosts[0].PublicAddress = netip.MustParseAddr("203.0.113.20")
	cluster.State.Hosts[2] = &state.Host{
		ID: 2, Name: "node2", Underlay: netip.MustParseAddr("10.0.0.4"), WGPublicKey: "wg2", Phase: state.Active,
	}
	// removed, but its address stays closed to containers
	cluster.State.Hosts[1] = &state.Host{
		ID: 1, Name: "node1", Underlay: netip.MustParseAddr("10.0.0.3"), WGPublicKey: "wg1", Phase: state.Removed,
	}

	wanted := readWantedNetwork(cluster, 0)
	want := wantedNetwork{
		mesh: network.Mesh{Self: 0, Peers: []network.Peer{
			{ID: 2, PublicKey: "wg2", Underlay: netip.MustParseAddr("10.0.0.4")},
		}},
		ruleset: network.Ruleset{
			FloatingIP: netip.MustParseAddr("203.0.113.10"),
			ClusterHosts: []netip.Addr{
				netip.MustParseAddr("10.0.0.2"), netip.MustParseAddr("10.0.0.3"), netip.MustParseAddr("10.0.0.4"),
				netip.MustParseAddr("203.0.113.20"),
			},
			PrivateNetwork: netip.MustParsePrefix("10.0.0.0/16"),
			Peers:          []state.HostID{2},
		},
	}
	if !reflect.DeepEqual(wanted, want) {
		t.Errorf("the wanted network:\n%+v\nwant\n%+v", wanted, want)
	}
}

func TestOneHostHasNoPeers(t *testing.T) {
	wanted := readWantedNetwork(agenttest.NewCluster(t), 0)
	if len(wanted.mesh.Peers) != 0 || len(wanted.ruleset.Peers) != 0 || len(wanted.ruleset.ClusterHosts) != 1 {
		t.Errorf("one host: %+v", wanted)
	}
}
