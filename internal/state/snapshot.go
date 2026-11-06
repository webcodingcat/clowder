package state

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
)

// SnapshotVersion is the format of the snapshots this build writes. As with
// commands, a snapshot of the previous version stays readable. New fields
// are optional, so the version only changes when a field changes its
// meaning.
const SnapshotVersion = 1

// snapshot is the file that Raft keeps in place of the log up to it.
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

// ReadSnapshot reads a state written by WriteSnapshot of this release or
// the previous one. Every error is an ErrUnreadable: like a log entry, a
// snapshot that the agent cannot read stops the agent.
func ReadSnapshot(in io.Reader) (*State, error) {
	compressed, err := gzip.NewReader(in)
	if err != nil {
		return nil, fmt.Errorf("the snapshot: %w: %w", ErrUnreadable, err)
	}
	defer func() { _ = compressed.Close() }() // a reader: nothing to flush
	var file snapshot
	if err := json.NewDecoder(compressed).Decode(&file); err != nil {
		return nil, fmt.Errorf("the snapshot: %w: %w", ErrUnreadable, err)
	}
	switch {
	case file.Version < max(1, SnapshotVersion-1) || file.Version > SnapshotVersion:
		return nil, fmt.Errorf("snapshot version %d is %w (it reads %d)", file.Version, ErrUnreadable, SnapshotVersion)
	case file.State == nil:
		return nil, fmt.Errorf("the snapshot holds no state: %w", ErrUnreadable)
	}
	return file.State, nil
}
