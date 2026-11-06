package storage

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/webcodingcat/clowder/internal/command"
)

// ErrNotYet is ForgetPeer's answer while a resource cannot forget the peer
// yet, because it is down or still has the peer configured. Both conditions
// clear by themselves, so the caller tries again without raising an alarm.
var ErrNotYet = errors.New("not yet")

// unknownPeer is the message that drbdsetup forget-peer prints when it
// refuses a node-id that has no slot (the kernel's ERR_INVALID_PEER_NODE_ID).
// Then there is nothing to forget. DRBD 9.3 prints this message only for a
// node-id out of range. For a node-id in range that the resource never knew
// or already forgot, it succeeds silently. The check stays for other
// versions of DRBD.
const unknownPeer = "invalid peer-node-id"

// ForgetPeer forgets a removed host as a peer of every DRBD resource here
// (drbdsetup forget-peer), so a host that joins later can use its node-id.
// The peer's slot in the metadata stays after the peer is removed. A new host
// with the same node-id would then meet the old peer's data generations.
// ForgetPeer fails, and the caller tries again, in three cases:
//   - a resource still has the peer configured (its drbdadm adjust removes
//     it);
//   - a resource is down (forget-peer works only on a resource that is up,
//     and the reconciler brings it up);
//   - drbdsetup refuses for any reason other than a peer that the resource
//     never knew.
func (d Driver) ForgetPeer(ctx context.Context, peer int) error {
	resources, err := d.resources(ctx)
	if err != nil {
		return err
	}
	files, err := filepath.Glob(filepath.Join(d.ResourceDir, "clw-*.res"))
	if err != nil {
		return fmt.Errorf("list the DRBD resource files: %w", err)
	}
	for _, file := range files {
		name := strings.TrimSuffix(filepath.Base(file), ".res")
		if _, up := resources[name]; !up {
			return fmt.Errorf("%w: resource %s is down; its metadata keeps peer %d until it is up", ErrNotYet, name, peer)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(resources)) {
		if err := d.forgetPeerOf(ctx, resources[name], peer); err != nil {
			return err
		}
	}
	return nil
}

// ForgetPeerOf forgets a peer that left one resource on this host. The
// peer's copy is gone. Without this step, its slot in the metadata would
// stay taken, and the next new copy might find no free slot (maxPeers). It
// fails like ForgetPeer, and with ErrNotYet while the resource is down.
func (d Driver) ForgetPeerOf(ctx context.Context, resource string, peer int) error {
	resources, err := d.resources(ctx)
	if err != nil {
		return err
	}
	up, ok := resources[resource]
	if !ok {
		return fmt.Errorf("%w: resource %s is down; its metadata keeps peer %d until it is up", ErrNotYet, resource, peer)
	}
	return d.forgetPeerOf(ctx, up, peer)
}

// forgetPeerOf skips a tiebreaker: it has no metadata, so nothing to forget.
func (d Driver) forgetPeerOf(ctx context.Context, resource drbdResource, peer int) error {
	if resource.disk() == DiskDiskless {
		return nil
	}
	if resource.hasPeer(peer) {
		return fmt.Errorf("%w: resource %s has peer %d still configured", ErrNotYet, resource.Name, peer)
	}
	_, err := d.Run(ctx, shortTimeout, "drbdsetup", "forget-peer", resource.Name, strconv.Itoa(peer))
	var exitError *command.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitError) && strings.Contains(strings.ToLower(exitError.Stderr), unknownPeer):
	default:
		return fmt.Errorf("forget peer %d on resource %s: %w", peer, resource.Name, err)
	}
	return nil
}
