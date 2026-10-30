package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/webcodingcat/clowder/internal/atomicfile"
	"github.com/webcodingcat/clowder/internal/storage"
)

// trimInterval is how often the volumes give free blocks back.
const trimInterval = 7 * 24 * time.Hour

// The record of a volume lists its copies on this host, one per generation
// that is still here: a file per volume, apart from the stacks, so a copy
// is found whatever happened to the stack that used it.

// record adds volume copies to their volumes' records.
func (r *stackRunner) record(volumes []storage.Volume) error {
	for _, volume := range volumes {
		recorded, err := r.readRecord(r.files.volumeRecord(volume.ID))
		if err != nil {
			return err
		}
		recorded = slices.DeleteFunc(recorded, func(old storage.Volume) bool { return old.LV == volume.LV })
		if err := r.writeRecord(volume.ID, append(recorded, volume)); err != nil {
			return err
		}
	}
	return nil
}

// writeRecord replaces a volume's record; without copies the file goes.
func (r *stackRunner) writeRecord(volumeID string, volumes []storage.Volume) error {
	file := r.files.volumeRecord(volumeID)
	if len(volumes) == 0 {
		if err := os.Remove(file); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("forget the copies of %s: %w", volumeID, err)
		}
		return nil
	}
	slices.SortFunc(volumes, func(a, b storage.Volume) int { return strings.Compare(a.LV, b.LV) })
	encoded, err := json.MarshalIndent(volumes, "", "  ")
	if err != nil {
		return fmt.Errorf("write the copies of %s: %w", volumeID, err)
	}
	return atomicfile.Write(file, encoded, 0o600)
}

func (r *stackRunner) readRecord(file string) ([]storage.Volume, error) {
	content, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", file, err)
	}
	var volumes []storage.Volume
	if err := json.Unmarshal(content, &volumes); err != nil {
		return nil, fmt.Errorf("read %s: %w", file, err)
	}
	return volumes, nil
}

// recordedVolumes are all volume copies on this host, by volume ID.
func (r *stackRunner) recordedVolumes() (map[string][]storage.Volume, error) {
	entries, err := os.ReadDir(r.files.volumeRecordsDir())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("list the volume copies of this host: %w", err)
	}
	volumes := map[string][]storage.Volume{}
	for _, entry := range entries {
		recorded, err := r.readRecord(filepath.Join(r.files.volumeRecordsDir(), entry.Name()))
		if err != nil {
			return nil, err
		}
		for _, volume := range recorded {
			volumes[volume.ID] = append(volumes[volume.ID], volume)
		}
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
	recorded, err := r.recordedVolumes()
	if err != nil {
		r.logger.Error("trim the volumes", "error", err)
		return
	}
	var volumes []storage.Volume
	for _, id := range slices.Sorted(maps.Keys(recorded)) {
		volumes = append(volumes, recorded[id]...)
	}
	if err := r.storage.Trim(ctx, volumes); err != nil {
		r.logger.Error("trim the volumes", "error", err)
	}
	// written after a failure too: the next try is in a week, not in an hour
	if err := atomicfile.Write(r.files.lastTrim(), nil, 0o600); err != nil {
		r.logger.Error("remember the trim", "error", err)
	}
}
