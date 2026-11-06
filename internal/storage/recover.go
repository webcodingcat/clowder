package storage

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
)

// DropPeers lets every DRBD resource that is up on this host continue
// without the peers, which are gone for good (clowderd recover). DRBD
// accepts the steps only in this order, as phase 0 showed: del-peer for each
// peer, then quorum off (resource-options refuses that while a peer is
// configured), then resume-io for a resource whose writes were suspended
// without quorum. Quorum stays off only until the agent adjusts the
// resource to its file, which lists this host alone. drbdsetup forget-peer
// is also left to the agent (ForgetPeer). A resource that is down has
// nothing to drop: it comes up from the agent's file. DropPeers may run
// again: it skips a peer that is already dropped, and a resource that is
// still suspended from a run that ended halfway gets quorum off and
// resume-io once more. resume-io goes through drbdadm, which finds the
// resource's device; drbdsetup would need its minor.
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
