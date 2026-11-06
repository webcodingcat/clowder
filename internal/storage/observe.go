package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Observed is the current state of a copy on the host.
type Observed struct {
	// LV says that the copy's LV exists. A tiebreaker has no LV. The
	// exception is a tiebreaker that used to have a disk: its LV exists
	// until its host removes it.
	LV bool
	// ResourceFileCurrent says that the DRBD resource file is the one the
	// copy should have (only for replicated volumes). A new peer changes the
	// file.
	ResourceFileCurrent bool
	// Role is the DRBD resource's role on this host (only for replicated
	// volumes).
	Role Role
	// Disk is DRBD's disk state of the copy on this host (DiskUpToDate,
	// ...). OutOfSyncBytes is the largest difference from any peer; during
	// a resync, it is the amount that is left. Both are set only for
	// replicated volumes, and only while the resource is up.
	Disk           string
	OutOfSyncBytes int64
	// Quorum says that the copy has DRBD's quorum. SplitBrain says that a
	// peer is in the StandAlone state (standAlonePeers). Both are set only
	// while the resource is up.
	Quorum, SplitBrain bool
	// Mounted says that a file system is mounted at the copy's mount point.
	// OtherDevice says that the mounted device is not the copy's device.
	Mounted, OtherDevice bool
}

// Synced tells whether the copy holds the volume's current data: DRBD
// says UpToDate, or a plain LV exists.
func (o Observed) Synced(volume Volume) bool {
	if volume.Resource == nil {
		return o.LV
	}
	return o.Disk == DiskUpToDate
}

// Observe reads the state of the copies on the host, keyed by LV. It only
// reads, and it reads for all copies at once: lvs per volume group, the
// state of all DRBD resources, and the mount table. So a pass that finds
// everything in order runs no tool per copy, and no tool that touches a
// volume.
func (d Driver) Observe(ctx context.Context, volumes []Volume) (map[string]Observed, error) {
	lvs := map[string]bool{} // by "<volume group>/<LV>"
	var groups []string
	replicated := false
	for _, volume := range volumes {
		if volume.Pool != "" {
			groups = append(groups, volumeGroup(volume.Pool))
		}
		replicated = replicated || volume.Resource != nil
	}
	for _, group := range slices.Compact(slices.Sorted(slices.Values(groups))) {
		names, err := d.listLVs(ctx, group)
		if err != nil {
			return nil, fmt.Errorf("list the LVs of %s: %w", group, err)
		}
		for _, name := range names {
			lvs[group+"/"+name] = true
		}
	}
	var resources map[string]drbdResource
	if replicated {
		var err error
		if resources, err = d.resources(ctx); err != nil {
			return nil, err
		}
	}
	mounts, err := d.mounts(ctx)
	if err != nil {
		return nil, err
	}
	observed := map[string]Observed{}
	for _, volume := range volumes {
		copyNow := Observed{LV: volume.Pool != "" && lvs[volumeGroup(volume.Pool)+"/"+volume.LV]}
		if volume.Resource != nil {
			resource := resources[volume.Resource.Name]
			copyNow.Role, copyNow.Disk, copyNow.OutOfSyncBytes = resource.Role, resource.disk(), resource.outOfSyncBytes()
			copyNow.Quorum, copyNow.SplitBrain = resource.quorum(), len(resource.standAlonePeers()) > 0
			if copyNow.ResourceFileCurrent, err = d.resourceFileCurrent(volume.Resource); err != nil {
				return nil, err
			}
		}
		source, mounted := mounts[d.MountPoint(volume)]
		copyNow.Mounted = mounted
		copyNow.OtherDevice = mounted && !sameDevice(source, d.device(volume))
		observed[volume.LV] = copyNow
	}
	return observed, nil
}

// mounts reads the mount table: the source of each mount point. --raw
// lists one mount per line and escapes blanks, so each line has two
// fields. Without a filter, the table always contains the root. So here
// findmnt's exit code 1 means a failure (a wrong argument also exits with
// 1), never "nothing mounted".
func (d Driver) mounts(ctx context.Context) (map[string]string, error) {
	output, err := d.Run(ctx, statusTimeout, "findmnt", "--raw", "--noheadings", "--output", "TARGET,SOURCE")
	if err != nil {
		return nil, fmt.Errorf("read the mounts: %w", err)
	}
	mounts := map[string]string{}
	for line := range strings.Lines(output) {
		if fields := strings.Fields(line); len(fields) == 2 {
			mounts[fields[0]] = fields[1]
		}
	}
	return mounts, nil
}

// sameDevice tells whether findmnt's source is the device. findmnt names an
// LV as /dev/mapper/<vg>-<lv>, and adds a bind mount's directory in
// brackets. So sameDevice drops the brackets and follows both paths to the
// device node they link to.
func sameDevice(source, device string) bool {
	source, _, _ = strings.Cut(source, "[")
	if source == device {
		return true
	}
	sourceNode, errSource := filepath.EvalSymlinks(source)
	deviceNode, errDevice := filepath.EvalSymlinks(device)
	return errSource == nil && errDevice == nil && sourceNode == deviceNode
}

// resourceFileCurrent tells whether the resource file has the wanted
// content and adjust has applied it.
func (d Driver) resourceFileCurrent(resource *Resource) (bool, error) {
	content, err := os.ReadFile(d.resourceFile(resource))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read the resource file of %s: %w", resource.Name, err)
	}
	if !bytes.Equal(content, []byte(RenderResource(*resource))) {
		return false, nil
	}
	// a file that adjust did not apply (it failed, for example without
	// quorum) is not current yet, so the next pass runs adjust again
	return d.hasMarker(resource.Name, "adjusted")
}
