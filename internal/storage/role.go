package storage

import (
	"context"
	"fmt"
)

// Role is a DRBD resource's role on this host.
type Role string

// The roles; RoleDown is a resource that is not up here.
const (
	RoleDown      Role = ""
	RoleSecondary Role = "Secondary"
	RolePrimary   Role = "Primary"
)

// Promote makes the resource primary here. force makes a copy primary that
// is not UpToDate: right only for a volume that never held data, whose
// first copy has no UpToDate peer to become. On a volume that holds data,
// it would make an outdated or empty copy the volume's truth.
func (d Driver) Promote(ctx context.Context, volume Volume, force bool) error {
	args := []string{"primary", volume.Resource.Name}
	if force {
		args = []string{"primary", "--force", volume.Resource.Name}
	}
	if _, err := d.Run(ctx, shortTimeout, "drbdadm", args...); err != nil {
		return fmt.Errorf("make %s primary here: %w", volume.ID, err)
	}
	return nil
}

// Demote makes the resource secondary here, so a copy on another host can
// become primary. DRBD refuses while the device is open (mounted).
func (d Driver) Demote(ctx context.Context, volume Volume) error {
	if _, err := d.Run(ctx, shortTimeout, "drbdadm", "secondary", volume.Resource.Name); err != nil {
		return fmt.Errorf("make %s secondary here: %w", volume.ID, err)
	}
	return nil
}
