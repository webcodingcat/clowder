package storage

import (
	"fmt"
	"strings"

	"github.com/webcodingcat/clowder/internal/network"
	"github.com/webcodingcat/clowder/internal/state"
)

// drbdBasePort is the port of minor 0; a resource listens on 10000 + minor
// on the node address (stackspec.ReservedPort keeps 10000-10999 free).
const drbdBasePort = 10000

// maxPeers is how many peers each resource's metadata has room for: three
// copies plus one slot for a move (each slot costs 32 KiB per GiB).
const maxPeers = 4

// Resource is one DRBD resource as /etc/drbd.d/<name>.res describes it. It
// is the same file on every host that has a copy.
type Resource struct {
	Name  string
	Minor int
	Nodes []Node
	// ResyncRate caps the resync per host (DRBD's c-max-rate), e.g. "50M".
	ResyncRate string
}

// Node is one host of a resource; an empty Disk is a diskless tiebreaker.
type Node struct {
	Host string
	ID   state.HostID
	Disk string
}

// RenderResource writes the resource file:
//   - quorum majority with suspend-io: a copy without quorum stops writing
//     instead of going its own way (split brain);
//   - auto-promote off: only the agent makes a copy primary;
//   - node-id is the host ID, the address the node address;
//   - the after-split-brain rules: discard what has no changes, never guess
//     on two primaries (a human decides).
func RenderResource(resource Resource) string {
	resyncRate := resource.ResyncRate
	if resyncRate == "" {
		resyncRate = "50M"
	}
	lines := []string{
		"# Written by clowderd; changes are overwritten.",
		"resource " + resource.Name + " {",
		"  options { quorum majority; on-no-quorum suspend-io; auto-promote no; }",
		"  net { protocol C; after-sb-0pri discard-zero-changes;",
		"        after-sb-1pri discard-secondary; after-sb-2pri disconnect; }",
		fmt.Sprintf("  disk { c-max-rate %s; c-plan-ahead 20; }", resyncRate),
		fmt.Sprintf("  device minor %d;", resource.Minor),
		"  meta-disk internal;",
	}
	var hosts []string
	for _, node := range resource.Nodes {
		disk := node.Disk
		if disk == "" {
			disk = "none"
		}
		lines = append(lines, fmt.Sprintf("  on %s { node-id %d; address %s:%d; disk %s; }",
			node.Host, node.ID, network.NodeAddress(node.ID), drbdBasePort+resource.Minor, disk))
		hosts = append(hosts, node.Host)
	}
	// one host alone has no connections
	if len(hosts) > 1 {
		lines = append(lines, "  connection-mesh { hosts "+strings.Join(hosts, " ")+"; }")
	}
	lines = append(lines, "}")
	return strings.Join(lines, "\n") + "\n"
}
