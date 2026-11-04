package storage

import (
	"context"
	"errors"
	"fmt"
	"slices"
)

// ErrNoSplitBrain says the copy has no peer that stands alone, so there is
// nothing to resolve here.
var ErrNoSplitBrain = errors.New("no peer of this copy is StandAlone")

// ResolveSplitBrain connects the copy again to the peers it stands alone
// from. With discard, this copy gives its changes since the split up and
// takes the peer's data (drbdadm connect --discard-my-data); it must be
// secondary for that. Without, it keeps its data and only connects, as the
// copy to keep and a tiebreaker do. A human decides which copy is kept:
// DRBD's rules could not.
func (d Driver) ResolveSplitBrain(ctx context.Context, volume Volume, discard bool) error {
	if volume.Resource == nil {
		return fmt.Errorf("volume %s has no DRBD resource", volume.ID)
	}
	resources, err := d.resources(ctx)
	if err != nil {
		return err
	}
	resource, up := resources[volume.Resource.Name]
	if !up {
		return fmt.Errorf("volume %s: the DRBD resource is down here", volume.ID)
	}
	peers := resource.standAlonePeers()
	if len(peers) == 0 {
		return fmt.Errorf("volume %s: %w", volume.ID, ErrNoSplitBrain)
	}
	if discard && resource.Role == RolePrimary {
		return fmt.Errorf("volume %s is primary here; its data cannot be discarded while a stack uses it", volume.ID)
	}
	for _, node := range volume.Resource.Nodes {
		if !slices.Contains(peers, int(node.ID)) {
			continue
		}
		args := []string{"connect"}
		if discard {
			args = append(args, "--discard-my-data")
		}
		// one connection, resource:peer: the others are connected already
		args = append(args, volume.Resource.Name+":"+node.Host)
		if _, err := d.Run(ctx, shortTimeout, "drbdadm", args...); err != nil {
			return fmt.Errorf("connect the copy of %s to %s again: %w", volume.ID, node.Host, err)
		}
	}
	return nil
}
