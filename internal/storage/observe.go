package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Observed is what the host has of a copy right now.
type Observed struct {
	// LV says the copy's LV exists.
	LV bool
	// ResourceFileCurrent says the DRBD resource file is the one the copy
	// should have (only with replicas); a new peer changes it.
	ResourceFileCurrent bool
	// Role is the DRBD resource's role here (only with replicas).
	Role Role
	// Mounted says a file system is mounted at the copy's mount point;
	// OtherDevice that it is not the copy's device.
	Mounted, OtherDevice bool
}

// Observe reads what the host has of the copies, by LV. It only reads, and
// at once for all copies: lvs per volume group, the roles of all DRBD
// resources and the mount table. So a pass that finds everything as it
// should be runs no tool per copy, and none that touches a volume.
func (d Driver) Observe(ctx context.Context, volumes []Volume) (map[string]Observed, error) {
	lvs := map[string]bool{} // by "<volume group>/<LV>"
	var groups []string
	replicated := false
	for _, volume := range volumes {
		groups = append(groups, volumeGroup(volume.Pool))
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
	var roles map[string]Role
	if replicated {
		var err error
		if roles, err = d.roles(ctx); err != nil {
			return nil, err
		}
	}
	mounts, err := d.mounts(ctx)
	if err != nil {
		return nil, err
	}
	observed := map[string]Observed{}
	for _, volume := range volumes {
		copyNow := Observed{LV: lvs[volumeGroup(volume.Pool)+"/"+volume.LV]}
		if volume.Resource != nil {
			copyNow.Role = roles[volume.Resource.Name]
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

// roles reads the role of every DRBD resource that is up here; one that is
// down is not in the list (RoleDown).
func (d Driver) roles(ctx context.Context) (map[string]Role, error) {
	output, err := d.Run(ctx, statusTimeout, "drbdsetup", "status", "--json")
	if err != nil {
		return nil, fmt.Errorf("read the DRBD resources: %w", err)
	}
	var resources []struct {
		Name string `json:"name"`
		Role Role   `json:"role"`
	}
	if strings.TrimSpace(output) != "" {
		if err := json.Unmarshal([]byte(output), &resources); err != nil {
			return nil, fmt.Errorf("read the DRBD resources: %w", err)
		}
	}
	roles := map[string]Role{}
	for _, resource := range resources {
		roles[resource.Name] = resource.Role
	}
	return roles, nil
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

// resourceFileCurrent tells whether the resource file says what it should.
func (d Driver) resourceFileCurrent(resource *Resource) (bool, error) {
	content, err := os.ReadFile(d.resourceFile(resource))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read the resource file of %s: %w", resource.Name, err)
	}
	return bytes.Equal(content, []byte(RenderResource(*resource))), nil
}
