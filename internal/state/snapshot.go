package state

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// SnapshotVersion is the format of the snapshots this build writes. Like
// commands, a snapshot of the version before stays readable; new fields are
// optional, so the version only changes when a field changes its meaning.
const SnapshotVersion = 1

// snapshot is the file Raft keeps instead of the log before it.
type snapshot struct {
	Version int    `json:"version"`
	State   *State `json:"state"`
}

// WriteSnapshot writes the state as gzip-compressed JSON.
func (s *State) WriteSnapshot(out io.Writer) error {
	compressed := gzip.NewWriter(out)
	if err := json.NewEncoder(compressed).Encode(snapshot{Version: SnapshotVersion, State: s}); err != nil {
		return fmt.Errorf("write the snapshot: %w", err)
	}
	if err := compressed.Close(); err != nil {
		return fmt.Errorf("write the snapshot: %w", err)
	}
	return nil
}

// Copy is a deep copy of the state without its audit, for work too slow to
// do under the state's lock (parsing Compose files) or that keeps parts of
// it. It goes through JSON, the way snapshots do.
func (s *State) Copy() (*State, error) {
	withoutAudit := *s
	withoutAudit.Audit = nil
	encoded, err := json.Marshal(&withoutAudit)
	if err != nil {
		return nil, fmt.Errorf("copy the state: %w", err)
	}
	var copied State
	if err := json.Unmarshal(encoded, &copied); err != nil {
		return nil, fmt.Errorf("copy the state: %w", err)
	}
	return &copied, nil
}

// ReadSnapshot reads a state written by WriteSnapshot of this release or the
// one before.
func ReadSnapshot(in io.Reader) (*State, error) {
	compressed, err := gzip.NewReader(in)
	if err != nil {
		return nil, fmt.Errorf("read the snapshot: %w", err)
	}
	defer func() { _ = compressed.Close() }() // a reader: nothing to flush
	var file snapshot
	if err := json.NewDecoder(compressed).Decode(&file); err != nil {
		return nil, fmt.Errorf("read the snapshot: %w", err)
	}
	switch {
	case file.Version < max(1, SnapshotVersion-1) || file.Version > SnapshotVersion:
		return nil, fmt.Errorf("snapshot version %d is not readable by this agent (%d)", file.Version, SnapshotVersion)
	case file.State == nil:
		return nil, errors.New("the snapshot holds no state")
	}
	return file.State, nil
}
