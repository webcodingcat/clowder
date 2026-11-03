package storage

import (
	"context"
	"encoding/json"
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

// unknownPeer is how drbdsetup forget-peer refuses a peer the resource has
// no slot for in its metadata (the kernel's ERR_INVALID_PEER_NODE_ID): it
// never knew the peer, so there is nothing to forget.
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
	resources, err := d.peers(ctx)
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
		if slices.Contains(resources[name], peer) {
			return fmt.Errorf("%w: resource %s has peer %d still configured", ErrNotYet, name, peer)
		}
		_, err := d.Run(ctx, shortTimeout, "drbdsetup", "forget-peer", name, strconv.Itoa(peer))
		var exitError *command.ExitError
		switch {
		case err == nil:
		case errors.As(err, &exitError) && strings.Contains(strings.ToLower(exitError.Stderr), unknownPeer):
		default:
			return fmt.Errorf("forget peer %d on resource %s: %w", peer, name, err)
		}
	}
	return nil
}

// peers reads the DRBD resources that are up here, each with the node-ids
// of its configured peers.
func (d Driver) peers(ctx context.Context) (map[string][]int, error) {
	output, err := d.Run(ctx, statusTimeout, "drbdsetup", "status", "--json")
	if err != nil {
		return nil, fmt.Errorf("read the DRBD resources: %w", err)
	}
	var resources []struct {
		Name        string `json:"name"`
		Connections []struct {
			PeerNodeID int `json:"peer-node-id"`
		} `json:"connections"`
	}
	if strings.TrimSpace(output) != "" {
		if err := json.Unmarshal([]byte(output), &resources); err != nil {
			return nil, fmt.Errorf("read the DRBD resources: %w", err)
		}
	}
	peers := map[string][]int{}
	for _, resource := range resources {
		peers[resource.Name] = []int{}
		for _, connection := range resource.Connections {
			peers[resource.Name] = append(peers[resource.Name], connection.PeerNodeID)
		}
	}
	return peers, nil
}
