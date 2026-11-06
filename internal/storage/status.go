package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// DiskUpToDate is DRBD's disk state of a copy that holds the volume's
// current data; DiskDiskless is the disk state of a tiebreaker.
const (
	DiskUpToDate = "UpToDate"
	DiskDiskless = "Diskless"
)

// drbdResource is one resource that is up on this host, as drbdsetup status
// --json shows it. A resource has one volume (device) and one connection per
// configured peer.
type drbdResource struct {
	Name string `json:"name"`
	Role Role   `json:"role"`
	// Suspended is true while DRBD holds the writes back, such as those of
	// a primary without quorum (on-no-quorum suspend-io)
	Suspended bool `json:"suspended"`
	Devices   []struct {
		DiskState string `json:"disk-state"`
		// Quorum is false while the copy on this host lacks DRBD's quorum;
		// a primary then suspends its writes (on-no-quorum suspend-io)
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
			// OutOfSync is how much this copy differs from the peer's, in
			// KiB
			OutOfSync int64 `json:"out-of-sync"`
		} `json:"peer_devices"`
	} `json:"connections"`
}

// disk is the resource's disk state on this host; empty without a device.
func (r drbdResource) disk() string {
	if len(r.Devices) == 0 {
		return ""
	}
	return r.Devices[0].DiskState
}

// outOfSyncBytes is the part of this copy's resync that is left: how much
// it differs from the peers that it resyncs from (SyncTarget, or paused).
// The other peers do not count: a peer that is cut off, or the tiebreaker,
// would make a healthy resync look stuck.
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

// resources reads the DRBD resources that are up on this host, keyed by
// name; a resource that is down is missing from the map.
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

// quorum tells whether the copy on this host has DRBD's quorum; false
// without a device.
func (r drbdResource) quorum() bool {
	return len(r.Devices) > 0 && r.Devices[0].Quorum
}

// standAlonePeers are the node-ids of the peers that the resource on this
// host no longer tries to connect to (StandAlone). This happens after a
// split brain that DRBD could not resolve by its rules, or after another
// refusal that needs a human.
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

// primaryPeer returns the node-id of a connected peer that is primary;
// false if there is none.
func (r drbdResource) primaryPeer() (int, bool) {
	for _, connection := range r.Connections {
		if connection.ConnectionState == "Connected" && connection.PeerRole == RolePrimary {
			return connection.PeerNodeID, true
		}
	}
	return 0, false
}
