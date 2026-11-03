package agent

import (
	"context"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

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

	wanted, known := readWantedNetwork(cluster, 0)
	if !known {
		t.Fatal("the state does not know host 0")
	}
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
	wanted, known := readWantedNetwork(agenttest.NewCluster(t), 0)
	if !known || len(wanted.mesh.Peers) != 0 || len(wanted.ruleset.Peers) != 0 || len(wanted.ruleset.ClusterHosts) != 1 {
		t.Errorf("one host: %+v", wanted)
	}
}

func TestAJoinedHostKeepsItsMeshUntilTheStateKnowsIt(t *testing.T) {
	// the state of a host that just joined: the log has not reached it yet
	empty := &agenttest.Cluster{State: &state.State{}}
	if _, known := readWantedNetwork(empty, 2); known {
		t.Error("an empty state names the network of host 2")
	}
	hostNetwork := &fakeNetwork{}
	files := Files{Dir: t.TempDir()}
	restored := network.Mesh{Self: 2, Peers: []network.Peer{{ID: 0, PublicKey: "wg", Underlay: netip.MustParseAddr("10.0.0.2")}}}
	if err := files.writeMesh(restored); err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		reconcileNetwork(ctx, empty, files, 2, hostNetwork, slog.New(slog.NewTextHandler(io.Discard, nil)))
		close(done)
	}()
	time.Sleep(100 * time.Millisecond) // one pass, which must do nothing
	stop()
	<-done
	hostNetwork.mu.Lock()
	defer hostNetwork.mu.Unlock()
	if len(hostNetwork.meshes) != 0 || len(hostNetwork.rulesets) != 0 {
		t.Errorf("applied %v and %v from an empty state", hostNetwork.meshes, hostNetwork.rulesets)
	}
	if err := restoreMesh(context.Background(), files, 2, &fakeNetwork{}); err != nil {
		t.Errorf("net.json after the pass: %v", err)
	}
	content, err := os.ReadFile(files.meshFile())
	if err != nil || !strings.Contains(string(content), `"public_key": "wg"`) {
		t.Errorf("net.json lost its peers: %s, %v", content, err)
	}
}
