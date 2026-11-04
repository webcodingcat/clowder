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
// yet: it is down, or still has the peer configured. Both pass by
// themselves, so the caller tries again without alarm.
var ErrNotYet = errors.New("not yet")

// unknownPeer is how drbdsetup forget-peer refuses a node-id it has no slot
// for (the kernel's ERR_INVALID_PEER_NODE_ID): there is nothing to forget.
// DRBD 9.3 says so only for a node-id out of range; one in range that the
// resource never knew, or forgot already, it forgets again without a word.
// The case stays for other versions of DRBD.
const unknownPeer = "invalid peer-node-id"

// ForgetPeer forgets a removed host as a peer of every DRBD resource here
// (drbdsetup forget-peer), so its node-id can serve a host that joins
// later: the peer's slot in the metadata outlives its removal, and a new
// host with the same node-id would meet the old peer's data generations.
// It fails, for the caller to try again, while a resource still has the
// peer configured (its drbdadm adjust takes it out), while a resource is
// down (forget-peer reaches only one that is up, and the reconciler brings
// it up), and for any refusal but that of a peer the resource never knew.
func (d Driver) ForgetPeer(ctx context.Context, peer int) error {
	resources, err := d.resources(ctx)
	if err != nil {
		return err
	}
	// a host without replicated volumes has no file: Glob finds none
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

// ForgetPeerOf forgets a peer that left one resource here: its copy is
// gone, and its slot in the metadata would be lost for the next copy that
// comes (maxPeers). It fails like ForgetPeer, and with ErrNotYet while the
// resource is down.
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

// forgetPeerOf runs forget-peer on a resource that is up, unless it still
// has the peer configured. A tiebreaker has no metadata, so it has nothing
// to forget.
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
