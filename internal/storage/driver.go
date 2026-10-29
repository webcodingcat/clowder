package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	// ID is "<stack>/<volume>"; the mount point is VolumesDir/<ID>.
	ID string
	// LV is the logical volume's name, Pool the thin pool ("vg/thin").
	LV        string
	Pool      string
	SizeBytes int64
	// Resource is the DRBD resource; nil for a volume with replicas 0, which
	// is a plain LV.
	Resource *Resource
}

// MountPoint is where the volume's file system is mounted; containers get
// its data/ directory.
func (d Driver) MountPoint(volume Volume) string {
	return filepath.Join(d.VolumesDir, filepath.FromSlash(volume.ID))
}

// Ensure brings the copy to what volume says and mounts it: the thin LV, the
// DRBD resource (new metadata only for a new one), ext4, the mount and data/.
// It does only what is missing, so the agent runs it at every start.
//
// The mount guard: data/ exists only inside the mounted file system. A
// container whose volume is not mounted fails to start instead of writing
// to the host's root file system.
func (d Driver) Ensure(ctx context.Context, volume Volume) error {
	created, err := d.ensureLV(ctx, volume)
	if err != nil {
		return err
	}
	device := "/dev/" + volumeGroup(volume.Pool) + "/" + volume.LV
	if volume.Resource != nil {
		if device, err = d.ensureResource(ctx, volume, created); err != nil {
			return err
		}
	}
	if err := d.ensureFilesystem(ctx, device); err != nil {
		return err
	}
	return d.ensureMounted(ctx, volume, device)
}

func volumeGroup(pool string) string {
	group, _, _ := strings.Cut(pool, "/")
	return group
}

// ensureLV creates the thin LV unless it is there; created says it is new.
func (d Driver) ensureLV(ctx context.Context, volume Volume) (bool, error) {
	path := volumeGroup(volume.Pool) + "/" + volume.LV
	if _, err := d.Run(ctx, shortTimeout, "lvs", path); err == nil {
		return false, nil
	}
	_, err := d.Run(ctx, shortTimeout, "lvcreate", "--quiet", "--thin", volume.Pool,
		"--virtualsize", strconv.FormatInt(volume.SizeBytes, 10)+"b", "--name", volume.LV)
	if err != nil {
		return false, fmt.Errorf("create the LV of %s: %w", volume.ID, err)
	}
	return true, nil
}

// ensureResource writes the resource file and brings the resource up as
// primary. New metadata is written only once, for a resource that has none
// yet; a marker remembers it, as an error of drbdadm up does not tell
// "no metadata" from other trouble.
func (d Driver) ensureResource(ctx context.Context, volume Volume, created bool) (string, error) {
	resource := volume.Resource
	file := filepath.Join(d.ResourceDir, resource.Name+".res")
	if err := atomicfile.Write(file, []byte(RenderResource(*resource)), 0o644); err != nil {
		return "", err
	}
	marker := filepath.Join(d.StateDir, resource.Name+".metadata")
	_, markerErr := os.Stat(marker)
	fresh := errors.Is(markerErr, os.ErrNotExist)
	if fresh && !created {
		return "", fmt.Errorf("volume %s: the LV exists but clowder never wrote its DRBD metadata; "+
			"look at it by hand before anything overwrites it", volume.ID)
	}
	if fresh {
		if _, err := d.Run(ctx, shortTimeout, "drbdadm", "create-md", "--force",
			"--max-peers="+strconv.Itoa(maxPeers), resource.Name); err != nil {
			return "", fmt.Errorf("write the DRBD metadata of %s: %w", volume.ID, err)
		}
		if err := atomicfile.Write(marker, []byte(volume.ID+"\n"), 0o600); err != nil {
			return "", err
		}
	}
	if _, err := d.Run(ctx, shortTimeout, "drbdadm", "adjust", resource.Name); err != nil {
		return "", fmt.Errorf("bring up the DRBD resource of %s: %w", volume.ID, err)
	}
	role, err := d.Run(ctx, shortTimeout, "drbdadm", "role", resource.Name)
	if err != nil {
		return "", fmt.Errorf("read the DRBD role of %s: %w", volume.ID, err)
	}
	if strings.TrimSpace(role) != "Primary" {
		primary := []string{"primary", resource.Name}
		if fresh {
			// a new resource has no UpToDate copy yet: this one is it
			primary = []string{"primary", "--force", resource.Name}
		}
		if _, err := d.Run(ctx, shortTimeout, "drbdadm", primary...); err != nil {
			return "", fmt.Errorf("make %s primary here: %w", volume.ID, err)
		}
	}
	return "/dev/drbd" + strconv.Itoa(resource.Minor), nil
}

// ensureFilesystem makes ext4 on a device that has no file system.
func (d Driver) ensureFilesystem(ctx context.Context, device string) error {
	// blkid exits with 2 when it finds no file system
	if kind, err := d.Run(ctx, shortTimeout, "blkid", "--output", "value", "--match-tag", "TYPE", device); err == nil {
		if strings.TrimSpace(kind) != "ext4" {
			return fmt.Errorf("%s holds %s, not ext4; clowder does not touch it", device, strings.TrimSpace(kind))
		}
		return nil
	}
	if _, err := d.Run(ctx, mkfsTimeout, "mkfs.ext4", "-q", device); err != nil {
		return fmt.Errorf("make ext4 on %s: %w", device, err)
	}
	return nil
}

// ensureMounted mounts the device and creates data/ inside it.
func (d Driver) ensureMounted(ctx context.Context, volume Volume, device string) error {
	mountPoint := d.MountPoint(volume)
	if err := os.MkdirAll(mountPoint, 0o700); err != nil {
		return fmt.Errorf("create the mount point of %s: %w", volume.ID, err)
	}
	if _, err := d.Run(ctx, shortTimeout, "findmnt", "--noheadings", "--mountpoint", mountPoint); err != nil {
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

// Remove unmounts the copy, takes the DRBD resource down and removes it and
// the LV with the data.
func (d Driver) Remove(ctx context.Context, volume Volume) error {
	mountPoint := d.MountPoint(volume)
	if _, err := d.Run(ctx, shortTimeout, "findmnt", "--noheadings", "--mountpoint", mountPoint); err == nil {
		if _, err := d.Run(ctx, shortTimeout, "umount", mountPoint); err != nil {
			return fmt.Errorf("unmount %s: %w", volume.ID, err)
		}
	}
	if volume.Resource != nil {
		name := volume.Resource.Name
		if _, err := d.Run(ctx, shortTimeout, "drbdadm", "down", name); err != nil {
			return fmt.Errorf("take down the DRBD resource of %s: %w", volume.ID, err)
		}
		for _, file := range []string{filepath.Join(d.ResourceDir, name+".res"), filepath.Join(d.StateDir, name+".metadata")} {
			if err := os.Remove(file); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	path := volumeGroup(volume.Pool) + "/" + volume.LV
	if _, err := d.Run(ctx, shortTimeout, "lvremove", "--yes", path); err != nil {
		return fmt.Errorf("remove the LV of %s: %w", volume.ID, err)
	}
	return os.Remove(mountPoint)
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
