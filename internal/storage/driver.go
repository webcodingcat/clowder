// Package storage keeps the volumes of this host: thin LVs in LVM thin
// pools, DRBD 9 resources for replicated volumes, ext4 and the mount under
// /var/lib/clowder/volumes. Every command that touches a volume runs with a
// timeout and is left behind if it hangs (internal/command): without quorum,
// DRBD suspends I/O.
//
// Each step of a copy's life is a method of its own, and each does only
// what is missing: Ensure makes the copy (LV, resource), Promote and Mount
// make it usable here, Unmount and Demote give it up, Remove takes it away.
// Which steps a copy needs, the agent decides from Observe; this file has
// the copy's making and removal, role.go the DRBD role, mount.go the file
// system and the mount, observe.go what the host has.
package storage

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/webcodingcat/clowder/internal/atomicfile"
)

// Timeouts of the commands; a command past its timeout is left behind.
const (
	shortTimeout  = time.Minute      // lvs, lvcreate, drbdadm, mount
	mkfsTimeout   = 10 * time.Minute // mkfs.ext4 of a large volume
	trimTimeout   = 30 * time.Minute // fstrim of a large volume
	statusTimeout = 10 * time.Second // reading the pools, the roles, the mounts
)

// Runner runs a command with a timeout (command.Run; tests record).
type Runner func(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error)

// Driver keeps this host's copies of volumes.
type Driver struct {
	// VolumesDir holds the mount points (/var/lib/clowder/volumes).
	VolumesDir string
	// StateDir holds what the driver remembers (/var/lib/clowder/storage).
	StateDir string
	// ResourceDir holds the DRBD resource files (/etc/drbd.d).
	ResourceDir string
	Run         Runner
}

// Volume is one copy of a volume on this host.
type Volume struct {
	// ID is "<namespace>/<volume>" (names.ID).
	ID string
	// LV is the logical volume's name, Pool the thin pool ("vg/thin"). The
	// name holds the generation, and so does the mount point VolumesDir/<LV>.
	LV        string
	Pool      string
	SizeBytes int64
	// Resource is the DRBD resource; nil for a volume with replicas 0, which
	// is a plain LV.
	Resource *Resource
	// Diskless is a tiebreaker: the resource without an LV, Pool empty. It
	// gives a volume with two copies its third vote and holds no data.
	Diskless bool
}

// MountPoint is where the volume's file system is mounted; containers get
// its data/ directory. Each generation has a mount point of its own: one
// shared by two generations would let the new one find the old one mounted
// there, and the removal of the old one unmount the new one.
func (d Driver) MountPoint(volume Volume) string {
	return filepath.Join(d.VolumesDir, volume.LV)
}

// device is the block device that holds the volume's file system: the DRBD
// device with replicas, the LV without.
func (d Driver) device(volume Volume) string {
	if volume.Resource != nil {
		return "/dev/drbd" + strconv.Itoa(volume.Resource.Minor)
	}
	return "/dev/" + volumeGroup(volume.Pool) + "/" + volume.LV
}

func volumeGroup(pool string) string {
	group, _, _ := strings.Cut(pool, "/")
	return group
}

func (d Driver) resourceFile(resource *Resource) string {
	return filepath.Join(d.ResourceDir, resource.Name+".res")
}

// Ensure makes the copy: the thin LV and, with replicas, the DRBD resource
// up with its metadata (new metadata only for a new one); a tiebreaker is
// the resource alone. A new resource
// comes up secondary; Ensure changes no role and mounts nothing, so a host
// can hold a copy that a stack elsewhere uses.
func (d Driver) Ensure(ctx context.Context, volume Volume) error {
	if volume.Diskless {
		return d.ensureResource(ctx, volume)
	}
	if err := d.ensureLV(ctx, volume); err != nil {
		return err
	}
	if volume.Resource == nil {
		return nil
	}
	return d.ensureResource(ctx, volume)
}

// The exit codes the driver reads. Any other failure of these tools (a
// timeout, a missing binary, a device not ready) stops the step: guessing
// "missing" there could create over or format real data.
const (
	blkidNotFound   = 2 // blkid: no file system on the device
	findmntNotFound = 1 // findmnt: nothing mounted that matches
)

// Markers in StateDir. They let Ensure pick up after a crash at any point:
//   - <LV>.created: clowder made this LV, so it is empty or clowder's own;
//   - <resource>.metadata: the DRBD metadata is written.
//
// Whether a volume holds data is not a marker: the state knows
// (Volume.Initialized), for every host.
func (d Driver) marker(name, kind string) string {
	return filepath.Join(d.StateDir, name+"."+kind)
}

func (d Driver) hasMarker(name, kind string) (bool, error) {
	_, err := os.Stat(d.marker(name, kind))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("read the marker %s: %w", d.marker(name, kind), err)
	}
}

func (d Driver) setMarker(name, kind, volumeID string) error {
	return atomicfile.Write(d.marker(name, kind), []byte(volumeID+"\n"), 0o600)
}

// ensureLV creates the thin LV unless it is there, and marks it as clowder's.
func (d Driver) ensureLV(ctx context.Context, volume Volume) error {
	exists, err := d.lvExists(ctx, volume)
	if err != nil || exists {
		return err
	}
	if _, err := d.Run(ctx, shortTimeout, "lvcreate", "--quiet", "--thin", volume.Pool,
		"--virtualsize", strconv.FormatInt(volume.SizeBytes, 10)+"b", "--name", volume.LV); err != nil {
		return fmt.Errorf("create the LV of %s: %w", volume.ID, err)
	}
	// a crash right here leaves an LV without the marker; Ensure then
	// refuses it and a human looks (it is empty)
	return d.setMarker(volume.LV, "created", volume.ID)
}

// lvExists tells whether the copy's LV exists: it looks for the name among
// the LVs of its volume group (listLVs).
func (d Driver) lvExists(ctx context.Context, volume Volume) (bool, error) {
	lvs, err := d.listLVs(ctx, volumeGroup(volume.Pool))
	if err != nil {
		return false, fmt.Errorf("look for the LV of %s: %w", volume.ID, err)
	}
	return slices.Contains(lvs, volume.LV), nil
}

// listLVs lists the names of a volume group's LVs. lvs has no exit code of
// its own for a missing LV (5 is any failure, a missing volume group or a
// lock too), so any failure is an error, never "missing".
func (d Driver) listLVs(ctx context.Context, group string) ([]string, error) {
	output, err := d.Run(ctx, shortTimeout, "lvs", "--noheadings", "--options", "lv_name", group)
	if err != nil {
		return nil, err
	}
	return strings.Fields(output), nil
}

// ensureResource writes the resource file and brings the resource up.
// New metadata goes only on an LV clowder made itself, and only once; a
// tiebreaker has none. The peers the new file no longer has are forgotten
// once adjust took them out (forgetDropped).
func (d Driver) ensureResource(ctx context.Context, volume Volume) error {
	resource := volume.Resource
	if err := d.noteDropped(resource); err != nil {
		return err
	}
	// the file is applied only once adjust worked (resourceFileCurrent)
	if err := os.Remove(d.marker(resource.Name, "adjusted")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("volume %s: %w", volume.ID, err)
	}
	if err := atomicfile.Write(d.resourceFile(resource), []byte(RenderResource(*resource)), 0o644); err != nil {
		return err
	}
	hasMetadata, err := d.hasMarker(resource.Name, "metadata")
	if err != nil {
		return err
	}
	if !hasMetadata && !volume.Diskless {
		ours, err := d.hasMarker(volume.LV, "created")
		if err != nil {
			return err
		}
		if !ours {
			return fmt.Errorf("volume %s: clowder did not create LV %s and never wrote its DRBD metadata; "+
				"look at it by hand before anything overwrites it", volume.ID, volume.LV)
		}
		if _, err := d.Run(ctx, shortTimeout, "drbdadm", "create-md", "--force",
			"--max-peers="+strconv.Itoa(maxPeers), resource.Name); err != nil {
			return fmt.Errorf("write the DRBD metadata of %s: %w", volume.ID, err)
		}
		if err := d.setMarker(resource.Name, "metadata", volume.ID); err != nil {
			return err
		}
	}
	// adjust brings a resource that is down up, and one that is up to what
	// the file says (new peers, a disk for a tiebreaker that gets one); it
	// keeps its role
	if _, err := d.Run(ctx, shortTimeout, "drbdadm", "adjust", resource.Name); err != nil {
		return fmt.Errorf("bring up the DRBD resource of %s: %w", volume.ID, err)
	}
	if err := d.setMarker(resource.Name, "adjusted", volume.ID); err != nil {
		return err
	}
	return d.forgetDropped(ctx, resource)
}

// noteDropped records the peers that the resource's file has and the new
// one does not, in the marker <resource>.forget, before the new file is
// written: so they are forgotten after a crash too.
func (d Driver) noteDropped(resource *Resource) error {
	old, err := os.ReadFile(d.resourceFile(resource))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the resource file of %s: %w", resource.Name, err)
	}
	dropped, err := d.droppedPeers(resource.Name)
	if err != nil {
		return err
	}
	for _, match := range nodeIDPattern.FindAllStringSubmatch(string(old), -1) {
		peer, _ := strconv.Atoi(match[1])
		kept := slices.ContainsFunc(resource.Nodes, func(node Node) bool { return int(node.ID) == peer })
		if !kept && !slices.Contains(dropped, peer) {
			dropped = append(dropped, peer)
		}
	}
	if len(dropped) == 0 {
		return nil
	}
	var lines []string
	for _, peer := range dropped {
		lines = append(lines, strconv.Itoa(peer))
	}
	return atomicfile.Write(d.marker(resource.Name, "forget"), []byte(strings.Join(lines, "\n")+"\n"), 0o600)
}

// nodeIDPattern finds the node-ids in a resource file (RenderResource).
var nodeIDPattern = regexp.MustCompile(`node-id (\d+);`)

// droppedPeers reads the peers the marker <resource>.forget names.
func (d Driver) droppedPeers(name string) ([]int, error) {
	content, err := os.ReadFile(d.marker(name, "forget"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the peers to forget of %s: %w", name, err)
	}
	var peers []int
	for _, field := range strings.Fields(string(content)) {
		peer, err := strconv.Atoi(field)
		if err != nil {
			return nil, fmt.Errorf("read the peers to forget of %s: %q is no node-id", name, field)
		}
		peers = append(peers, peer)
	}
	return peers, nil
}

// forgetDropped forgets the peers a resource dropped (noteDropped), so the
// next copy that comes finds a free slot in the metadata (maxPeers).
func (d Driver) forgetDropped(ctx context.Context, resource *Resource) error {
	dropped, err := d.droppedPeers(resource.Name)
	if err != nil || len(dropped) == 0 {
		return err
	}
	for _, peer := range dropped {
		if err := d.ForgetPeerOf(ctx, resource.Name, peer); err != nil {
			return err
		}
	}
	if err := os.Remove(d.marker(resource.Name, "forget")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("forget the dropped peers of %s: %w", resource.Name, err)
	}
	return nil
}

// Remove unmounts the copy, takes the DRBD resource down and removes it and
// the LV with the data. It picks up after a crash at any point: what is gone
// already is skipped.
func (d Driver) Remove(ctx context.Context, volume Volume) error {
	if err := d.Unmount(ctx, volume); err != nil {
		return err
	}
	leftovers := []string{d.marker(volume.LV, "created")}
	if volume.Resource != nil {
		name := volume.Resource.Name
		resourceFile := d.resourceFile(volume.Resource)
		// without its file drbdadm knows the resource no more: it was down
		// before the file went. Any other error leaves it unknown whether the
		// resource is up, and the LV below it must not go then.
		_, err := os.Stat(resourceFile)
		switch {
		case err == nil:
			if err := d.checkNoPrimaryPeer(ctx, volume); err != nil {
				return err
			}
			if _, err := d.Run(ctx, shortTimeout, "drbdadm", "down", name); err != nil {
				return fmt.Errorf("take down the DRBD resource of %s: %w", volume.ID, err)
			}
		case !errors.Is(err, fs.ErrNotExist):
			return fmt.Errorf("look for the DRBD resource of %s: %w", volume.ID, err)
		}
		leftovers = append(leftovers, resourceFile, d.marker(name, "metadata"), d.marker(name, "forget"),
			d.marker(name, "adjusted"))
	}
	if volume.Diskless {
		return removeFiles(append(leftovers, d.MountPoint(volume)), volume.ID)
	}
	exists, err := d.lvExists(ctx, volume)
	if err != nil {
		return err
	}
	if exists {
		path := volumeGroup(volume.Pool) + "/" + volume.LV
		if _, err := d.Run(ctx, shortTimeout, "lvremove", "--yes", path); err != nil {
			return fmt.Errorf("remove the LV of %s: %w", volume.ID, err)
		}
	}
	return removeFiles(append(leftovers, d.MountPoint(volume)), volume.ID)
}

// removeFiles removes what is left of a copy; what is gone already is
// skipped.
func removeFiles(files []string, volumeID string) error {
	for _, file := range files {
		if err := os.Remove(file); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s of %s: %w", file, volumeID, err)
		}
	}
	return nil
}

// Trim gives the free blocks of a mounted copy back to its thin pool
// (fstrim), so deleted files make room again. A copy that is not mounted is
// skipped: fstrim on an empty mount point would trim the root file system.
func (d Driver) Trim(ctx context.Context, volume Volume) error {
	isMounted, err := d.mounted(ctx, d.MountPoint(volume))
	if err != nil || !isMounted {
		return err
	}
	if _, err := d.Run(ctx, trimTimeout, "fstrim", d.MountPoint(volume)); err != nil {
		return fmt.Errorf("trim %s: %w", volume.ID, err)
	}
	return nil
}

// PoolUsage reads a thin pool's size and how full its data and metadata
// are, in percent.
func (d Driver) PoolUsage(ctx context.Context, pool string) (sizeBytes int64, dataPercent, metadataPercent float64, err error) {
	output, err := d.Run(ctx, statusTimeout, "lvs", "--noheadings", "--units", "b", "--nosuffix",
		"--separator", ",", "--options", "lv_size,data_percent,metadata_percent", pool)
	if err != nil {
		return 0, 0, 0, err
	}
	fields := strings.Split(strings.TrimSpace(output), ",")
	if len(fields) != 3 {
		return 0, 0, 0, fmt.Errorf("lvs %s: unexpected %q", pool, output)
	}
	if sizeBytes, err = strconv.ParseInt(strings.TrimSpace(fields[0]), 10, 64); err != nil {
		return 0, 0, 0, fmt.Errorf("lvs %s: size %q: %w", pool, fields[0], err)
	}
	if dataPercent, err = strconv.ParseFloat(strings.TrimSpace(fields[1]), 64); err != nil {
		return 0, 0, 0, fmt.Errorf("lvs %s: data %q: %w", pool, fields[1], err)
	}
	if metadataPercent, err = strconv.ParseFloat(strings.TrimSpace(fields[2]), 64); err != nil {
		return 0, 0, 0, fmt.Errorf("lvs %s: metadata %q: %w", pool, fields[2], err)
	}
	return sizeBytes, dataPercent, metadataPercent, nil
}

// checkNoPrimaryPeer refuses to take a copy down while a peer that is
// primary is still connected to it (ErrNotYet): with quorum majority, the
// primary of a resource with two copies would lose its quorum and suspend
// the stack's writes. The primary goes first (a deleted volume), or drops
// this copy from its resource (adjust, a copy given up); either ends the
// connection.
func (d Driver) checkNoPrimaryPeer(ctx context.Context, volume Volume) error {
	resources, err := d.resources(ctx)
	if err != nil {
		return err
	}
	if peer, ok := resources[volume.Resource.Name].primaryPeer(); ok {
		return fmt.Errorf("%w: peer %d is primary and still connected to the copy of %s", ErrNotYet, peer, volume.ID)
	}
	return nil
}
