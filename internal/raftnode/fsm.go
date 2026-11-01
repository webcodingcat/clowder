package raftnode

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/hashicorp/raft"

	"github.com/webcodingcat/clowder/internal/state"
)

// fsm hands Raft's log entries to the cluster state. Raft calls Apply,
// Snapshot and Restore from one goroutine; everyone else reads under the
// lock.
type fsm struct {
	mu    sync.RWMutex
	state *state.State
	// changes are what the applied entries changed, for the watch; only
	// the log fills it, so a dry run never does
	changes state.ChangeRing
	// changed is closed and replaced after every applied entry, to wake up
	// those waiting for an index.
	changed chan struct{}
	// shared is a copy of the state for the loops to read, made once per
	// change and dropped at the next (sharedCopy); sharedMu guards it among
	// readers, who hold mu only to read
	sharedMu sync.Mutex
	shared   *state.State
}

func newFSM() *fsm {
	return &fsm{state: &state.State{}, changed: make(chan struct{})}
}

// Apply applies one command. Its error goes back to the proposer as the
// response; for Raft the entry is applied either way.
func (f *fsm) Apply(entry *raft.Log) any {
	f.mu.Lock()
	// defers run backwards: unlock first, then wake, which takes the lock
	// again to swap the channel
	defer f.wake()
	defer f.mu.Unlock()
	changes, err := f.state.ApplyEntry(entry.Index, entry.Data)
	f.changes.Add(changes)
	f.shared = nil
	return err
}

// wake tells the waiters that the state changed.
func (f *fsm) wake() {
	f.mu.Lock()
	close(f.changed)
	f.changed = make(chan struct{})
	f.mu.Unlock()
}

// Snapshot encodes the state at once: Apply goes on while Raft persists the
// snapshot, and the state is small enough to copy.
func (f *fsm) Snapshot() (raft.FSMSnapshot, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	var encoded bytes.Buffer
	if err := f.state.WriteSnapshot(&encoded); err != nil {
		return nil, err
	}
	return snapshot(encoded.Bytes()), nil
}

// Restore replaces the state with a snapshot's.
func (f *fsm) Restore(snapshotFile io.ReadCloser) error {
	defer func() { _ = snapshotFile.Close() }() // read only
	restored, err := state.ReadSnapshot(snapshotFile)
	if err != nil {
		return err
	}
	f.mu.Lock()
	f.state = restored
	// what came before the snapshot is in it, not in the ring
	f.changes.Reset(restored.Index)
	f.shared = nil
	f.mu.Unlock()
	f.wake()
	return nil
}

// changesSince returns the changes after index and the state's index.
func (f *fsm) changesSince(index uint64) ([]state.Change, uint64, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	changes, err := f.changes.Since(index)
	return changes, f.state.Index, err
}

// read runs a function on the state under the read lock.
func (f *fsm) read(use func(current *state.State)) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	use(f.state)
}

// sharedCopy returns a copy of the state that every caller shares until the
// next change: the loops that wake at a change each read the state without
// the lock, and one copy serves them all. Nobody may change it.
func (f *fsm) sharedCopy() (*state.State, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	f.sharedMu.Lock()
	defer f.sharedMu.Unlock()
	if f.shared == nil {
		copied, err := f.state.Copy()
		if err != nil {
			return nil, err
		}
		f.shared = copied
	}
	return f.shared, nil
}

// waitFor returns once the state has applied index.
func (f *fsm) waitFor(ctx context.Context, index uint64) error {
	for {
		f.mu.RLock()
		reached, changed := f.state.Index >= index, f.changed
		f.mu.RUnlock()
		if reached {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return fmt.Errorf("wait for index %d: %w", index, ctx.Err())
		}
	}
}

// revoked tells whether a certificate serial is on the state's list.
func (f *fsm) revoked(serial string) bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	_, revoked := f.state.Revoked[serial]
	return revoked
}

// snapshot is an encoded state waiting to be persisted.
type snapshot []byte

func (s snapshot) Persist(sink raft.SnapshotSink) error {
	if _, err := sink.Write(s); err != nil {
		_ = sink.Cancel() // the write error is the one to report
		return fmt.Errorf("persist the snapshot: %w", err)
	}
	return sink.Close()
}

func (s snapshot) Release() {}
