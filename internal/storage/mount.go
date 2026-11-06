package storage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/webcodingcat/clowder/internal/command"
)

// Mount mounts the copy's file system and creates data/ inside it. format
// allows a new file system on a device that has none, which is right only
// for a volume that never held data. Otherwise a missing file system means
// that the data is not on this copy, and a human must check it.
//
// The mount guard: data/ exists only inside the mounted file system. A
// container whose volume is not mounted fails to start instead of writing
// to the host's root file system.
func (d Driver) Mount(ctx context.Context, volume Volume, format bool) error {
	if err := d.ensureFilesystem(ctx, volume, format); err != nil {
		return err
	}
	return d.ensureMounted(ctx, volume)
}

// ensureFilesystem creates ext4 on a device that has no file system, and
// only if three things hold: blkid reports "no file system" with its exit
// code, format allows it, and clowder created the LV below it. A foreign LV
// without a file system may hold raw data.
func (d Driver) ensureFilesystem(ctx context.Context, volume Volume, format bool) error {
	device := d.device(volume)
	kind, err := d.Run(ctx, shortTimeout, "blkid", "--output", "value", "--match-tag", "TYPE", device)
	code, exited := command.ExitCode(err)
	switch {
	case err == nil && strings.TrimSpace(kind) == "ext4":
		return nil
	case err == nil:
		return fmt.Errorf("%s holds %s, not ext4; clowder does not touch it", device, strings.TrimSpace(kind))
	case !exited || code != blkidNotFound:
		return fmt.Errorf("look for a file system on %s: %w", device, err)
	case !format:
		return fmt.Errorf("volume %s holds data, but %s has no file system: "+
			"this copy lacks the data, a human must look", volume.ID, device)
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

func (d Driver) ensureMounted(ctx context.Context, volume Volume) error {
	device, mountPoint := d.device(volume), d.MountPoint(volume)
	isMounted, err := d.mounted(ctx, mountPoint)
	if err != nil {
		return err
	}
	if isMounted {
		// the mounted device must be this copy's device; otherwise another
		// device's data would reach the containers as the volume's data
		_, err := d.Run(ctx, shortTimeout, "findmnt", "--noheadings", "--source", device, "--mountpoint", mountPoint)
		if code, exited := command.ExitCode(err); exited && code == findmntNotFound {
			return fmt.Errorf("%s has another device mounted, not %s: a human must look", mountPoint, device)
		}
		if err != nil {
			return fmt.Errorf("look what is mounted for %s: %w", volume.ID, err)
		}
	} else {
		// the agent touches the mount point only while nothing is mounted
		// there, because a mounted volume's root could hang on suspended DRBD
		if err := os.MkdirAll(mountPoint, 0o700); err != nil {
			return fmt.Errorf("create the mount point of %s: %w", volume.ID, err)
		}
		if _, err := d.Run(ctx, shortTimeout, "mount", "-t", "ext4", device, mountPoint); err != nil {
			return fmt.Errorf("mount %s: %w", volume.ID, err)
		}
	}
	// a write to the volume runs in a separate process, not in the agent,
	// so a hang stays in that process
	if _, err := d.Run(ctx, shortTimeout, "mkdir", "-p", filepath.Join(mountPoint, "data")); err != nil {
		return fmt.Errorf("create data/ in %s: %w", volume.ID, err)
	}
	return nil
}

// Unmount unmounts the copy's file system, unless nothing is mounted.
func (d Driver) Unmount(ctx context.Context, volume Volume) error {
	mountPoint := d.MountPoint(volume)
	isMounted, err := d.mounted(ctx, mountPoint)
	if err != nil || !isMounted {
		return err
	}
	if _, err := d.Run(ctx, shortTimeout, "umount", mountPoint); err != nil {
		return fmt.Errorf("unmount %s: %w", volume.ID, err)
	}
	return nil
}

func (d Driver) mounted(ctx context.Context, mountPoint string) (bool, error) {
	_, err := d.Run(ctx, shortTimeout, "findmnt", "--noheadings", "--mountpoint", mountPoint)
	if code, exited := command.ExitCode(err); exited && code == findmntNotFound {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("look whether %s is mounted: %w", mountPoint, err)
	}
	return true, nil
}
