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

// Observed is what the host has of a copy right now.
type Observed struct {
	// LV says the copy's LV exists; a tiebreaker has none, but for the one
	// of a copy that had a disk until its host removes it.
	LV bool
	// ResourceFileCurrent says the DRBD resource file is the one the copy
	// should have (only with replicas); a new peer changes it.
	ResourceFileCurrent bool
	// Role is the DRBD resource's role here (only with replicas).
	Role Role
	// Disk is DRBD's disk state of the copy here (DiskUpToDate, ...), and
	// OutOfSyncBytes how much it differs by from a peer at most: while it
	// resyncs, what is left. Both only with replicas, and only while the
	// resource is up.
	Disk           string
	OutOfSyncBytes int64
	// Quorum says the copy has DRBD's quorum, and SplitBrain that a peer
	// stands alone (standAlonePeers); both only while the resource is up.
	Quorum, SplitBrain bool
	// Mounted says a file system is mounted at the copy's mount point;
	// OtherDevice that it is not the copy's device.
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

// Observe reads what the host has of the copies, by LV. It only reads, and
// at once for all copies: lvs per volume group, the state of all DRBD
// resources and the mount table. So a pass that finds everything as it
// should be runs no tool per copy, and none that touches a volume.
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
// fields. Unfiltered, the table always has the root, so findmnt's exit code
// 1 is a failure here (a wrong argument exits with 1 too), never "nothing
// mounted".
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
// LV by /dev/mapper/<vg>-<lv> and a bind mount with its directory in
// brackets, so both are followed to the device node they link to.
func sameDevice(source, device string) bool {
	source, _, _ = strings.Cut(source, "[")
	if source == device {
		return true
	}
	sourceNode, errSource := filepath.EvalSymlinks(source)
	deviceNode, errDevice := filepath.EvalSymlinks(device)
	return errSource == nil && errDevice == nil && sourceNode == deviceNode
}

// resourceFileCurrent tells whether the resource file says what it should,
// and adjust applied it.
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
	// a file adjust did not apply (it failed, say for want of quorum) is
	// not current yet: the next pass runs adjust again
	return d.hasMarker(resource.Name, "adjusted")
}
