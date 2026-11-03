package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/webcodingcat/clowder/internal/state"
)

// peersHost answers the tools as a host that has the resource clw-data up,
// with what status says.
type peersHost struct {
	status   string
	commands []string
}

func (p *peersHost) run(_ context.Context, _ time.Duration, name string, args ...string) (string, error) {
	line := name + " " + strings.Join(args, " ")
	p.commands = append(p.commands, line)
	switch {
	case line == "drbdsetup status --json":
		return p.status, nil
	case name == "findmnt":
		return "/ /dev/sda1\n", nil
	}
	return "", nil
}

// dataResource is clw-data on the hosts given, host 2 a tiebreaker.
func dataResource(hosts ...int) *Resource {
	resource := &Resource{Name: "clw-data", Minor: 5}
	for _, id := range hosts {
		node := Node{Host: "node" + strconv.Itoa(id), ID: state.HostID(id), Disk: "/dev/vg/clw-data"}
		if id == 2 {
			node.Disk = ""
		}
		resource.Nodes = append(resource.Nodes, node)
	}
	return resource
}

func newPeersDriver(t *testing.T, host *peersHost) Driver {
	t.Helper()
	dir := t.TempDir()
	return Driver{
		VolumesDir: filepath.Join(dir, "volumes"), StateDir: dir, ResourceDir: dir, Run: host.run,
	}
}

func TestATiebreakerIsTheResourceAloneWithoutLVOrMetadata(t *testing.T) {
	host := &peersHost{status: "[]"}
	driver := newPeersDriver(t, host)
	tiebreaker := Volume{ID: "default/data", LV: "clw-data", Resource: dataResource(0, 1, 2), Diskless: true}
	if err := driver.Ensure(context.Background(), tiebreaker); err != nil {
		t.Fatal(err)
	}
	if want := []string{"drbdadm adjust clw-data"}; !slices.Equal(host.commands, want) {
		t.Errorf("ensure ran %q", host.commands)
	}
	host.commands = nil
	if err := driver.Remove(context.Background(), tiebreaker); err != nil {
		t.Fatal(err)
	}
	for _, line := range host.commands {
		if strings.HasPrefix(line, "lv") {
			t.Errorf("the removal of a tiebreaker ran %q", line)
		}
	}
	if _, err := os.Stat(driver.resourceFile(tiebreaker.Resource)); !os.IsNotExist(err) {
		t.Errorf("the resource file is left: %v", err)
	}
}

func TestAPeerTheResourceDropsIsForgottenAfterTheAdjust(t *testing.T) {
	host := &peersHost{status: `[{"name": "clw-data", "devices": [{"disk-state": "UpToDate"}],
		"connections": [{"peer-node-id": 1}]}]`}
	driver := newPeersDriver(t, host)
	volume := Volume{ID: "default/data", LV: "clw-data", Resource: dataResource(0, 1, 3)}
	if err := os.WriteFile(driver.resourceFile(volume.Resource), []byte(RenderResource(*volume.Resource)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := driver.setMarker("clw-data", "metadata", volume.ID); err != nil {
		t.Fatal(err)
	}
	// host 3's copy went; host 2 came as the tiebreaker
	volume.Resource = dataResource(0, 1, 2)
	if err := driver.ensureResource(context.Background(), volume); err != nil {
		t.Fatal(err)
	}
	want := []string{"drbdadm adjust clw-data", "drbdsetup status --json", "drbdsetup forget-peer clw-data 3"}
	if !slices.Equal(host.commands, want) {
		t.Errorf("commands %q", host.commands)
	}
	if _, err := os.Stat(driver.marker("clw-data", "forget")); !os.IsNotExist(err) {
		t.Errorf("the peers to forget stay noted: %v", err)
	}
}

func TestADroppedPeerIsNotedUntilItIsForgotten(t *testing.T) {
	// the resource is down: forget-peer cannot reach it yet
	host := &peersHost{status: "[]"}
	driver := newPeersDriver(t, host)
	volume := Volume{ID: "default/data", LV: "clw-data", Resource: dataResource(0, 1, 3)}
	if err := os.WriteFile(driver.resourceFile(volume.Resource), []byte(RenderResource(*volume.Resource)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := driver.setMarker("clw-data", "metadata", volume.ID); err != nil {
		t.Fatal(err)
	}
	volume.Resource = dataResource(0, 1)
	if err := driver.ensureResource(context.Background(), volume); err == nil {
		t.Fatal("a peer forgotten on a resource that is down")
	}
	// the file is new now, the peer stays noted for the next ensure
	host.status = `[{"name": "clw-data", "devices": [{"disk-state": "UpToDate"}], "connections": []}]`
	host.commands = nil
	if err := driver.ensureResource(context.Background(), volume); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(host.commands, "drbdsetup forget-peer clw-data 3") {
		t.Errorf("commands %q", host.commands)
	}
}

func TestObserveReadsTheDiskStateAndWhatIsLeftOfTheResyncFromItsSource(t *testing.T) {
	host := &peersHost{status: `[{"name": "clw-data", "role": "Secondary", "devices": [{"disk-state": "Inconsistent"}],
		"connections": [{"peer-node-id": 1, "peer_devices": [{"replication-state": "SyncTarget", "out-of-sync": 2048}]},
			{"peer-node-id": 2, "peer_devices": [{"replication-state": "Established", "out-of-sync": 4096}]}]}]`}
	driver := newPeersDriver(t, host)
	volume := Volume{ID: "default/data", LV: "clw-data", Pool: "vg/thin", Resource: dataResource(0, 1, 2)}
	observed, err := driver.Observe(context.Background(), []Volume{volume})
	if err != nil {
		t.Fatal(err)
	}
	got := observed["clw-data"]
	if got.Disk != "Inconsistent" || got.OutOfSyncBytes != 2048*1024 || got.Synced(volume) {
		t.Errorf("observed %+v", got)
	}
}

func TestACopyGoesDownOnlyOnceNoPrimaryPeerIsConnectedToIt(t *testing.T) {
	host := &peersHost{status: `[{"name": "clw-data", "role": "Secondary", "devices": [{"disk-state": "UpToDate"}],
		"connections": [{"peer-node-id": 1, "connection-state": "Connected", "peer-role": "Primary"}]}]`}
	driver := newPeersDriver(t, host)
	volume := Volume{ID: "default/data", LV: "clw-data", Pool: "vg/thin", Resource: dataResource(0, 1)}
	if err := os.WriteFile(driver.resourceFile(volume.Resource), []byte(RenderResource(*volume.Resource)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := driver.Remove(context.Background(), volume); !errors.Is(err, ErrNotYet) {
		t.Fatalf("a copy with a primary peer connected: %v", err)
	}
	if slices.Contains(host.commands, "drbdadm down clw-data") {
		t.Fatal("the copy went down under its primary")
	}
	// the primary dropped it: the connection ends
	host.status = `[{"name": "clw-data", "role": "Secondary", "devices": [{"disk-state": "UpToDate"}],
		"connections": [{"peer-node-id": 1, "connection-state": "Connecting", "peer-role": "Unknown"}]}]`
	if err := driver.Remove(context.Background(), volume); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(host.commands, "drbdadm down clw-data") {
		t.Errorf("commands %q", host.commands)
	}
}
