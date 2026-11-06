package stacks

import (
	"context"
	"errors"
	"os"

	"github.com/webcodingcat/clowder/internal/clowderd/daemon/loop"
)

// stopLeavers returns the volumes that leavers which failed to stop may
// still use. A leaver's compose.yaml stays until compose down succeeds, so
// that a crash at any point leaves it to be stopped again.
func (r *Runner) stopLeavers(ctx context.Context, desired desiredState) volumesInUse {
	inUse := volumesInUse{}
	entries, err := os.ReadDir(r.files.StacksDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		// without knowing the leavers, any of them may use any volume
		r.logger.Error("list the stacks of this host", "error", err)
		inUse[anyVolume] = "a stack this host could not list"
		return inUse
	}
	for _, entry := range entries {
		project := entry.Name()
		if _, staying := desired.stacks[project]; staying {
			continue
		}
		if r.markStaleIfBusy(project) {
			// its job may still be running compose up. The pass after the
			// job stops the stack.
			r.markInUse(inUse, project)
			continue
		}
		if err := r.stop(ctx, project); err != nil {
			r.logger.Error("stop a stack that left this host", "project", project, "error", err)
			r.markInUse(inUse, project)
			continue
		}
		if err := os.RemoveAll(r.files.stackDir(project)); err != nil {
			r.logger.Error("forget a stack", "project", project, "error", err)
			continue
		}
		// the stack no longer holds its volumes' copies, so a volume deleted
		// with it in one apply is removed now, not at the reconciler's next
		// interval
		if r.volumesReleased != nil {
			r.volumesReleased()
		}
	}
	return inUse
}

func (r *Runner) stop(ctx context.Context, project string) error {
	file := r.files.stackFile(project)
	if _, err := os.Stat(file); errors.Is(err, os.ErrNotExist) {
		return nil // stopped before
	}
	err := loop.WhilePaused(r.supervisor, project, func() error { return r.docker.ComposeDown(ctx, project, file) })
	if err != nil {
		return err
	}
	r.forgetApplied(project)
	return os.Remove(file)
}

// volumesInUse maps each LV that a stack which failed to stop may still use
// to that stack's project. anyVolume stands for every LV.
type volumesInUse map[string]string

// anyVolume is in use when the runner cannot tell which volumes a leaver
// uses.
const anyVolume = "*"

var errVolumeInUse = errors.New("a stack that failed to stop may still use a volume")

func (u volumesInUse) user(lv string) (string, bool) {
	if project, ok := u[lv]; ok {
		return project, true
	}
	project, ok := u[anyVolume]
	return project, ok
}

// markInUse marks every volume if the leaver's LVs cannot be read: starting
// a stack next to the leaver by mistake could ruin a file system, and
// waiting costs only a pass.
func (r *Runner) markInUse(inUse volumesInUse, project string) {
	if _, err := os.Stat(r.files.stackFile(project)); errors.Is(err, os.ErrNotExist) {
		return // stopped after all
	}
	lvs, err := r.files.readMountedLVs(project)
	if err != nil {
		r.logger.Error("read the volumes of a stack that failed to stop", "project", project, "error", err)
		inUse[anyVolume] = project
		return
	}
	for _, lv := range lvs {
		inUse[lv] = project
	}
}
