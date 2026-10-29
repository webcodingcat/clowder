// Package storage keeps the volumes of this host: thin LVs in LVM thin
// pools, DRBD 9 resources for replicated volumes, ext4 and the mount under
// /var/lib/clowder/volumes. Every command that touches a volume runs with a
// timeout and is left behind if it hangs (internal/command): without quorum,
// DRBD suspends I/O.
package storage

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/webcodingcat/clowder/internal/atomicfile"
	"github.com/webcodingcat/clowder/internal/command"
)

// Timeouts of the commands; a command past its timeout is left behind.
const (
	shortTimeout  = time.Minute      // lvs, lvcreate, drbdadm, mount
	mkfsTimeout   = 10 * time.Minute // mkfs.ext4 of a large volume
	trimTimeout   = 30 * time.Minute // fstrim of a large volume
	statusTimeout = 10 * time.Second // reading the pools
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
	// ID is "<stack>/<volume>".
	ID string
	// LV is the logical volume's name, Pool the thin pool ("vg/thin"). The
	// name holds the generation, and so does the mount point VolumesDir/<LV>.
	LV        string
	Pool      string
	SizeBytes int64
	// Resource is the DRBD resource; nil for a volume with replicas 0, which
	// is a plain LV.
	Resource *Resource
}

// MountPoint is where the volume's file system is mounted; containers get
// its data/ directory. Each generation has a mount point of its own: one
// shared by two generations would let the new one find the old one mounted
// there, and the removal of the old one unmount the new one.
func (d Driver) MountPoint(volume Volume) string {
	return filepath.Join(d.VolumesDir, volume.LV)
}

// Ensure brings the copy to what volume says and mounts it: the thin LV, the
// DRBD resource (new metadata only for a new one), ext4, the mount and data/.
// It does only what is missing, so the agent runs it at every start.
//
// The mount guard: data/ exists only inside the mounted file system. A
// container whose volume is not mounted fails to start instead of writing
// to the host's root file system.
func (d Driver) Ensure(ctx context.Context, volume Volume) error {
	if err := d.ensureLV(ctx, volume); err != nil {
		return err
	}
	device := "/dev/" + volumeGroup(volume.Pool) + "/" + volume.LV
	if volume.Resource != nil {
		var err error
		if device, err = d.ensureResource(ctx, volume); err != nil {
			return err
		}
	}
	if err := d.ensureFilesystem(ctx, volume, device); err != nil {
		return err
	}
	return d.ensureMounted(ctx, volume, device)
}

func volumeGroup(pool string) string {
	group, _, _ := strings.Cut(pool, "/")
	return group
}

// The exit codes the driver reads. Any other failure of these tools (a
// timeout, a missing binary, a device not ready) stops Ensure: guessing
// "missing" there could create over or format real data.
const (
	lvsNotFound   = 5 // lvs: no such LV
	blkidNotFound = 2 // blkid: no file system on the device
)

// Markers in StateDir. They let Ensure pick up after a crash at any point:
//   - <LV>.created: clowder made this LV, so it is empty or clowder's own;
//   - <resource>.metadata: the DRBD metadata is written;
//   - <resource>.primary: the first promotion worked (a new resource is made
//     primary with --force, as it has no UpToDate copy yet).
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

// lvExists tells whether the copy's LV exists; only lvs's own "not found"
// means no, any other failure is an error.
func (d Driver) lvExists(ctx context.Context, volume Volume) (bool, error) {
	_, err := d.Run(ctx, shortTimeout, "lvs", volumeGroup(volume.Pool)+"/"+volume.LV)
	if code, ok := command.ExitCode(err); err != nil && ok && code == lvsNotFound {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("look for the LV of %s: %w", volume.ID, err)
	}
	return true, nil
}

// ensureResource writes the resource file and brings the resource up as
// primary. New metadata goes only on an LV clowder made itself, and only
// once.
func (d Driver) ensureResource(ctx context.Context, volume Volume) (string, error) {
	resource := volume.Resource
	file := filepath.Join(d.ResourceDir, resource.Name+".res")
	if err := atomicfile.Write(file, []byte(RenderResource(*resource)), 0o644); err != nil {
		return "", err
	}
	hasMetadata, err := d.hasMarker(resource.Name, "metadata")
	if err != nil {
		return "", err
	}
	if !hasMetadata {
		ours, err := d.hasMarker(volume.LV, "created")
		if err != nil {
			return "", err
		}
		if !ours {
			return "", fmt.Errorf("volume %s: clowder did not create LV %s and never wrote its DRBD metadata; "+
				"look at it by hand before anything overwrites it", volume.ID, volume.LV)
		}
		if _, err := d.Run(ctx, shortTimeout, "drbdadm", "create-md", "--force",
			"--max-peers="+strconv.Itoa(maxPeers), resource.Name); err != nil {
			return "", fmt.Errorf("write the DRBD metadata of %s: %w", volume.ID, err)
		}
		if err := d.setMarker(resource.Name, "metadata", volume.ID); err != nil {
			return "", err
		}
	}
	if _, err := d.Run(ctx, shortTimeout, "drbdadm", "adjust", resource.Name); err != nil {
		return "", fmt.Errorf("bring up the DRBD resource of %s: %w", volume.ID, err)
	}
	if err := d.ensurePrimary(ctx, volume); err != nil {
		return "", err
	}
	return "/dev/drbd" + strconv.Itoa(resource.Minor), nil
}

// ensurePrimary makes the resource primary here unless it is.
func (d Driver) ensurePrimary(ctx context.Context, volume Volume) error {
	name := volume.Resource.Name
	role, err := d.Run(ctx, shortTimeout, "drbdadm", "role", name)
	if err != nil {
		return fmt.Errorf("read the DRBD role of %s: %w", volume.ID, err)
	}
	if strings.TrimSpace(role) == "Primary" {
		return nil
	}
	promoted, err := d.hasMarker(name, "primary")
	if err != nil {
		return err
	}
	args := []string{"primary", name}
	if !promoted {
		// a new resource has no UpToDate copy yet: this one becomes it
		args = []string{"primary", "--force", name}
	}
	if _, err := d.Run(ctx, shortTimeout, "drbdadm", args...); err != nil {
		return fmt.Errorf("make %s primary here: %w", volume.ID, err)
	}
	return d.setMarker(name, "primary", volume.ID)
}

// ensureFilesystem makes ext4 on a device that has no file system, and only
// then: blkid must say so with its exit code, and clowder must have made the
// LV below it. A foreign LV without a file system may hold raw data.
func (d Driver) ensureFilesystem(ctx context.Context, volume Volume, device string) error {
	kind, err := d.Run(ctx, shortTimeout, "blkid", "--output", "value", "--match-tag", "TYPE", device)
	code, exited := command.ExitCode(err)
	switch {
	case err == nil && strings.TrimSpace(kind) == "ext4":
		return nil
	case err == nil:
		return fmt.Errorf("%s holds %s, not ext4; clowder does not touch it", device, strings.TrimSpace(kind))
	case !exited || code != blkidNotFound:
		return fmt.Errorf("look for a file system on %s: %w", device, err)
	}
	ours, err := d.hasMarker(volume.LV, "created")
	if err != nil {
		return err
	}
	if !ours {
		return fmt.Errorf("volume %s: clowder did not create LV %s, which has no file system; "+
			"look at it by hand before anything formats it", volume.ID, volume.LV)
	}
	if _, err := d.Run(ctx, mkfsTimeout, "mkfs.ext4", "-q", device); err != nil {
		return fmt.Errorf("make ext4 on %s: %w", device, err)
	}
	return nil
}

// ensureMounted mounts the device and creates data/ inside it.
func (d Driver) ensureMounted(ctx context.Context, volume Volume, device string) error {
	mountPoint := d.MountPoint(volume)
	isMounted, err := d.mounted(ctx, mountPoint)
	if err != nil {
		return err
	}
	if !isMounted {
		// the agent touches the mount point only while nothing is mounted
		// there: a mounted volume's root could hang on suspended DRBD
		if err := os.MkdirAll(mountPoint, 0o700); err != nil {
			return fmt.Errorf("create the mount point of %s: %w", volume.ID, err)
		}
		if _, err := d.Run(ctx, shortTimeout, "mount", "-t", "ext4", device, mountPoint); err != nil {
			return fmt.Errorf("mount %s: %w", volume.ID, err)
		}
	}
	// a write to the volume: a process, not the agent, so a hang stays there
	if _, err := d.Run(ctx, shortTimeout, "mkdir", "-p", filepath.Join(mountPoint, "data")); err != nil {
		return fmt.Errorf("create data/ in %s: %w", volume.ID, err)
	}
	return nil
}

// mounted tells whether a file system is mounted at mountPoint; findmnt
// exits with 1 when none is.
func (d Driver) mounted(ctx context.Context, mountPoint string) (bool, error) {
	_, err := d.Run(ctx, shortTimeout, "findmnt", "--noheadings", "--mountpoint", mountPoint)
	if code, exited := command.ExitCode(err); exited && code == 1 {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("look whether %s is mounted: %w", mountPoint, err)
	}
	return true, nil
}

// Remove unmounts the copy, takes the DRBD resource down and removes it and
// the LV with the data. It picks up after a crash at any point: what is gone
// already is skipped.
func (d Driver) Remove(ctx context.Context, volume Volume) error {
	mountPoint := d.MountPoint(volume)
	isMounted, err := d.mounted(ctx, mountPoint)
	if err != nil {
		return err
	}
	if isMounted {
		if _, err := d.Run(ctx, shortTimeout, "umount", mountPoint); err != nil {
			return fmt.Errorf("unmount %s: %w", volume.ID, err)
		}
	}
	leftovers := []string{d.marker(volume.LV, "created")}
	if volume.Resource != nil {
		name := volume.Resource.Name
		resourceFile := filepath.Join(d.ResourceDir, name+".res")
		// without its file drbdadm knows the resource no more: it was down
		// before the file went. Any other error leaves it unknown whether the
		// resource is up, and the LV below it must not go then.
		_, err := os.Stat(resourceFile)
		switch {
		case err == nil:
			if _, err := d.Run(ctx, shortTimeout, "drbdadm", "down", name); err != nil {
				return fmt.Errorf("take down the DRBD resource of %s: %w", volume.ID, err)
			}
		case !errors.Is(err, fs.ErrNotExist):
			return fmt.Errorf("look for the DRBD resource of %s: %w", volume.ID, err)
		}
		leftovers = append(leftovers, resourceFile, d.marker(name, "metadata"), d.marker(name, "primary"))
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
	for _, file := range append(leftovers, mountPoint) {
		if err := os.Remove(file); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s of %s: %w", file, volume.ID, err)
		}
	}
	return nil
}

// Trim gives the free blocks of the mounted volumes back to their thin
// pools (fstrim), so deleted files make room again. Volumes that fail are
// reported, the others trimmed.
func (d Driver) Trim(ctx context.Context, volumes []Volume) error {
	var errs []error
	for _, volume := range volumes {
		if _, err := d.Run(ctx, trimTimeout, "fstrim", d.MountPoint(volume)); err != nil {
			errs = append(errs, fmt.Errorf("trim %s: %w", volume.ID, err))
		}
	}
	return errors.Join(errs...)
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
