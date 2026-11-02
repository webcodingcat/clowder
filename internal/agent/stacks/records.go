package stacks

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/webcodingcat/clowder/internal/atomicfile"
	"github.com/webcodingcat/clowder/internal/storage"
)

// The record of a volume lists its copies on this host, one per generation
// that is still here: a file per volume, apart from the stacks, so a copy
// is found whatever happened to the stack that used it.

// recordCopies adds volume copies to their volumes' records.
func (r *Runner) recordCopies(volumes []storage.Volume) error {
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
func (r *Runner) writeRecord(volumeID string, volumes []storage.Volume) error {
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

// readRecord reads a volume's record; a missing file has no copies.
func (r *Runner) readRecord(file string) ([]storage.Volume, error) {
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

// recordedCopies are all volume copies on this host, by volume ID.
func (r *Runner) recordedCopies() (map[string][]storage.Volume, error) {
	entries, err := os.ReadDir(r.files.VolumeRecordsDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("list the volume copies of this host: %w", err)
	}
	volumes := map[string][]storage.Volume{}
	for _, entry := range entries {
		recorded, err := r.readRecord(filepath.Join(r.files.VolumeRecordsDir, entry.Name()))
		if err != nil {
			return nil, err
		}
		for _, volume := range recorded {
			volumes[volume.ID] = append(volumes[volume.ID], volume)
		}
	}
	return volumes, nil
}
