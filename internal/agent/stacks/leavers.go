package stacks

import (
	"context"
	"errors"
	"os"

	"github.com/webcodingcat/clowder/internal/agent/loop"
)

// stopLeavers stops the stacks that left this host. The files tell how far
// a leaver got, so a crash at any point is picked up: its compose.yaml
// exists until compose down worked (stop), then its directory goes, and its
// secrets go once compose.yaml is gone (removeSecretsOfOthers). Its volume
// copies have records of their own (removeVolumes). It returns the volumes
// that leavers which failed to stop may still use.
func (r *Runner) stopLeavers(ctx context.Context, desired desiredState) volumesInUse {
	inUse := volumesInUse{}
	entries, err := os.ReadDir(r.files.StacksDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		// not knowing the leavers, any of them may use any volume
		r.logger.Error("list the stacks of this host", "error", err)
		inUse[anyVolume] = "a stack this host could not list"
		return inUse
	}
	for _, entry := range entries {
		project := entry.Name()
		if _, staying := desired.stacks[project]; staying {
			continue
		}
		if err := r.stop(ctx, project); err != nil {
			r.logger.Error("stop a stack that left this host", "project", project, "error", err)
			r.markInUse(inUse, project)
			continue
		}
		if err := os.RemoveAll(r.files.stackDir(project)); err != nil {
			r.logger.Error("forget a stack", "project", project, "error", err)
		}
	}
	return inUse
}

// stop runs compose down for a stack that left this host, once: its
// Compose file goes with it.
func (r *Runner) stop(ctx context.Context, project string) error {
	file := r.files.stackFile(project)
	if _, err := os.Stat(file); errors.Is(err, os.ErrNotExist) {
		return nil // stopped before
	}
	err := loop.WhilePaused(r.supervisor, project, func() error { return r.docker.ComposeDown(ctx, project, file) })
	if err != nil {
		return err
	}
	delete(r.applied, project)
	return os.Remove(file)
}

// volumesInUse are the LVs that stacks which failed to stop may still use,
// with the stack's project; anyVolume stands for every LV.
type volumesInUse map[string]string

// anyVolume is in use when a leaver's volumes cannot be told.
const anyVolume = "*"

// errVolumeInUse is apply's answer for a stack that must wait: a stack that
// failed to stop may still use one of its volumes.
var errVolumeInUse = errors.New("a stack that failed to stop may still use a volume")

// user tells which stack may still use an LV.
func (u volumesInUse) user(lv string) (string, bool) {
	if project, ok := u[lv]; ok {
		return project, true
	}
	project, ok := u[anyVolume]
	return project, ok
}

// markInUse adds the LVs a leaver's containers may hold (readMountedLVs):
// what it may still use while they run. If they cannot be read, it marks
// every volume, as wrongly starting a stack beside it could ruin a file
// system, and waiting costs only a pass.
func (r *Runner) markInUse(inUse volumesInUse, project string) {
	if _, err := os.Stat(r.files.stackFile(project)); errors.Is(err, os.ErrNotExist) {
		return // stopped after all
	}
	lvs, err := r.readMountedLVs(project)
	if err != nil {
		r.logger.Error("read the volumes of a stack that failed to stop", "project", project, "error", err)
		inUse[anyVolume] = project
		return
	}
	for _, lv := range lvs {
		inUse[lv] = project
	}
}
