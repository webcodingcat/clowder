package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/webcodingcat/clowder/internal/atomicfile"
	"github.com/webcodingcat/clowder/internal/storage"
)

// trimInterval is how often the volumes give free blocks back.
const trimInterval = 7 * 24 * time.Hour

// record adds volume copies to a stack's record of the copies on this host.
func (r *stackRunner) record(stackName string, volumes []storage.Volume) error {
	recorded, err := r.readRecord(stackName)
	if err != nil {
		return err
	}
	for _, volume := range volumes {
		recorded = slices.DeleteFunc(recorded, func(old storage.Volume) bool { return old.LV == volume.LV })
		recorded = append(recorded, volume)
	}
	return r.writeRecord(stackName, recorded)
}

func (r *stackRunner) writeRecord(stackName string, volumes []storage.Volume) error {
	slices.SortFunc(volumes, func(a, b storage.Volume) int { return strings.Compare(a.LV, b.LV) })
	encoded, err := json.MarshalIndent(volumes, "", "  ")
	if err != nil {
		return fmt.Errorf("write the volumes of %s: %w", stackName, err)
	}
	return atomicfile.Write(r.files.volumesRecord(stackName), encoded, 0o600)
}

func (r *stackRunner) readRecord(stackName string) ([]storage.Volume, error) {
	content, err := os.ReadFile(r.files.volumesRecord(stackName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the volumes of %s: %w", stackName, err)
	}
	var volumes []storage.Volume
	if err := json.Unmarshal(content, &volumes); err != nil {
		return nil, fmt.Errorf("read the volumes of %s: %w", stackName, err)
	}
	return volumes, nil
}

// recordedVolumes are all volume copies on this host.
func (r *stackRunner) recordedVolumes() ([]storage.Volume, error) {
	entries, err := os.ReadDir(r.files.stacksDir())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("list the stacks of this host: %w", err)
	}
	var volumes []storage.Volume
	for _, entry := range entries {
		recorded, err := r.readRecord(entry.Name())
		if err != nil {
			return nil, err
		}
		volumes = append(volumes, recorded...)
	}
	return volumes, nil
}

// trim gives the free blocks of this host's volumes back to their pools
// once every trimInterval, until ctx ends. The time of the last trim is a
// file, so restarts of the agent do not put it off.
func (r *stackRunner) trim(ctx context.Context) {
	for {
		info, err := os.Stat(r.files.lastTrim())
		if err != nil || time.Since(info.ModTime()) >= trimInterval {
			r.trimNow(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Hour):
		}
	}
}

func (r *stackRunner) trimNow(ctx context.Context) {
	volumes, err := r.recordedVolumes()
	if err != nil {
		r.logger.Error("trim the volumes", "error", err)
		return
	}
	if err := r.storage.Trim(ctx, volumes); err != nil {
		r.logger.Error("trim the volumes", "error", err)
	}
	// written after a failure too: the next try is in a week, not in an hour
	if err := atomicfile.Write(r.files.lastTrim(), nil, 0o600); err != nil {
		r.logger.Error("remember the trim", "error", err)
	}
}
