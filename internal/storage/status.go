package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// DiskUpToDate is DRBD's disk state of a copy that holds the volume's
// current data; DiskDiskless that of a tiebreaker.
const (
	DiskUpToDate = "UpToDate"
	DiskDiskless = "Diskless"
)

// drbdResource is one resource that is up here, as drbdsetup status --json
// shows it: a resource has one volume (device), and a connection per
// configured peer.
type drbdResource struct {
	Name    string `json:"name"`
	Role    Role   `json:"role"`
	Devices []struct {
		DiskState string `json:"disk-state"`
		// Quorum is false while the copy here lacks DRBD's quorum: a
		// primary then suspends its writes (on-no-quorum suspend-io)
		Quorum bool `json:"quorum"`
	} `json:"devices"`
	Connections []struct {
		PeerNodeID      int    `json:"peer-node-id"`
		ConnectionState string `json:"connection-state"`
		PeerRole        Role   `json:"peer-role"`
		PeerDevices     []struct {
			// ReplicationState is SyncTarget while this copy resyncs from
			// the peer
			ReplicationState string `json:"replication-state"`
			// OutOfSync is what this copy and the peer's differ by, in KiB
			OutOfSync int64 `json:"out-of-sync"`
		} `json:"peer_devices"`
	} `json:"connections"`
}

// disk is the resource's disk state here; empty without a device.
func (r drbdResource) disk() string {
	if len(r.Devices) == 0 {
		return ""
	}
	return r.Devices[0].DiskState
}

// outOfSyncBytes is what is left of this copy's resync: what it differs
// by from the peers it resyncs from (SyncTarget, or paused). The other
// peers do not count: one cut off, or the tiebreaker, would make a
// healthy resync look stuck.
func (r drbdResource) outOfSyncBytes() int64 {
	most := int64(0)
	for _, connection := range r.Connections {
		for _, peerDevice := range connection.PeerDevices {
			if peerDevice.ReplicationState != "SyncTarget" && peerDevice.ReplicationState != "PausedSyncT" {
				continue
			}
			most = max(most, peerDevice.OutOfSync*1024)
		}
	}
	return most
}

// resources reads the DRBD resources that are up here, by name; one that
// is down is not among them.
func (d Driver) resources(ctx context.Context) (map[string]drbdResource, error) {
	output, err := d.Run(ctx, statusTimeout, "drbdsetup", "status", "--json")
	if err != nil {
		return nil, fmt.Errorf("read the DRBD resources: %w", err)
	}
	var listed []drbdResource
	if strings.TrimSpace(output) != "" {
		if err := json.Unmarshal([]byte(output), &listed); err != nil {
			return nil, fmt.Errorf("read the DRBD resources: %w", err)
		}
	}
	resources := map[string]drbdResource{}
	for _, resource := range listed {
		resources[resource.Name] = resource
	}
	return resources, nil
}

// quorum tells whether the copy here has DRBD's quorum; false without a
// device.
func (r drbdResource) quorum() bool {
	return len(r.Devices) > 0 && r.Devices[0].Quorum
}

// standAlonePeers are the peers, by node-id, that the resource here no
// longer tries to connect to (StandAlone): after a split brain DRBD could
// not resolve by its rules, or another refusal that needs a human.
func (r drbdResource) standAlonePeers() []int {
	var peers []int
	for _, connection := range r.Connections {
		if connection.ConnectionState == "StandAlone" {
			peers = append(peers, connection.PeerNodeID)
		}
	}
	return peers
}

// hasPeer tells whether the resource has the peer configured.
func (r drbdResource) hasPeer(peer int) bool {
	for _, connection := range r.Connections {
		if connection.PeerNodeID == peer {
			return true
		}
	}
	return false
}

// primaryPeer is a connected peer that is primary, by its node-id; false
// if there is none.
func (r drbdResource) primaryPeer() (int, bool) {
	for _, connection := range r.Connections {
		if connection.ConnectionState == "Connected" && connection.PeerRole == RolePrimary {
			return connection.PeerNodeID, true
		}
	}
	return 0, false
}
