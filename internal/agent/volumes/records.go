package volumes

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/webcodingcat/clowder/internal/atomicfile"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/storage"
)

// The record of a volume lists its copies on this host, one for each
// generation that is still here. Each volume has its own file, so a copy is
// found whatever happened to the state's record of it. A copy is recorded
// before it is created, and its entry is deleted only after the copy is
// removed.
// Only the volume's job writes its record (Reconciler.busy).

func (r *Reconciler) recordFile(volumeID string) string {
	return filepath.Join(r.files.RecordsDir, names.FileName(volumeID)+".json")
}

// recordCopy adds a copy to its volume's record, or replaces the entry of
// the same LV.
func (r *Reconciler) recordCopy(volume storage.Volume) error {
	recorded, err := r.readRecord(r.recordFile(volume.ID))
	if err != nil {
		return err
	}
	recorded = slices.DeleteFunc(recorded, func(old storage.Volume) bool { return old.LV == volume.LV })
	return r.writeRecord(volume.ID, append(recorded, volume))
}

// forgetCopy takes a removed copy out of its volume's record.
func (r *Reconciler) forgetCopy(volume storage.Volume) error {
	recorded, err := r.readRecord(r.recordFile(volume.ID))
	if err != nil {
		return err
	}
	recorded = slices.DeleteFunc(recorded, func(old storage.Volume) bool { return old.LV == volume.LV })
	return r.writeRecord(volume.ID, recorded)
}

// writeRecord replaces a volume's record. Without copies, it removes the
// file.
func (r *Reconciler) writeRecord(volumeID string, volumes []storage.Volume) error {
	file := r.recordFile(volumeID)
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

// readRecord reads a volume's record. A missing file means no copies.
func (r *Reconciler) readRecord(file string) ([]storage.Volume, error) {
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

// recordedCopies returns all recorded volume copies on this host, by volume ID.
func (r *Reconciler) recordedCopies() (map[string][]storage.Volume, error) {
	entries, err := os.ReadDir(r.files.RecordsDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("list the volume copies of this host: %w", err)
	}
	volumes := map[string][]storage.Volume{}
	for _, entry := range entries {
		recorded, err := r.readRecord(filepath.Join(r.files.RecordsDir, entry.Name()))
		if err != nil {
			return nil, err
		}
		for _, volume := range recorded {
			volumes[volume.ID] = append(volumes[volume.ID], volume)
		}
	}
	return volumes, nil
}
