package storage

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
)

// DropPeers lets every DRBD resource that is up on this host continue
// without the given peers, which are gone for good (clowderd recover). DRBD
// accepts the steps only in this order:
//  1. del-peer for each peer;
//  2. quorum off (resource-options refuses it while a peer is configured);
//  3. resume-io for a resource whose writes were suspended.
//
// Quorum stays off only until the agent adjusts the resource to its file,
// which lists this host alone. The agent also runs forget-peer later
// (ForgetPeer). A resource that is down comes up from the agent's file.
//
// DropPeers may run again. It skips a peer that is already dropped, and it
// repeats quorum off and resume-io on a resource that is still suspended.
// resume-io goes through drbdadm, because drbdsetup would need the device's
// minor.
func (d Driver) DropPeers(ctx context.Context, peers []int) error {
	resources, err := d.resources(ctx)
	if err != nil {
		return err
	}
	for _, name := range slices.Sorted(maps.Keys(resources)) {
		resource := resources[name]
		dropped := false
		for _, peer := range peers {
			if !resource.hasPeer(peer) {
				continue
			}
			if _, err := d.Run(ctx, shortTimeout, "drbdsetup", "del-peer", name, strconv.Itoa(peer)); err != nil {
				return fmt.Errorf("drop peer %d of DRBD resource %s: %w", peer, name, err)
			}
			dropped = true
		}
		if !dropped && !resource.Suspended {
			continue
		}
		if _, err := d.Run(ctx, shortTimeout, "drbdsetup", "resource-options", name, "--quorum=off"); err != nil {
			return fmt.Errorf("turn the quorum of DRBD resource %s off: %w", name, err)
		}
		if resource.Suspended {
			if _, err := d.Run(ctx, shortTimeout, "drbdadm", "resume-io", name); err != nil {
				return fmt.Errorf("resume the writes of DRBD resource %s: %w", name, err)
			}
		}
	}
	return nil
}
