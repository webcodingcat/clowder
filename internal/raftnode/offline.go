package raftnode

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"
	"go.etcd.io/bbolt"
	bolterrors "go.etcd.io/bbolt/errors"

	"github.com/webcodingcat/clowder/internal/state"
)

// storeLockTimeout is how long ReadState and Recover wait for the Raft
// store: a running agent holds it.
const storeLockTimeout = 5 * time.Second

// ReadState reads the state as this member's log on disk has it, without
// Raft: the last snapshot, then every command after it. That includes
// commands the cluster may never have committed, as Recover takes them
// too. The agent must not run (it holds the store). A member that never
// started has an empty state.
func ReadState(dir string) (*state.State, error) {
	if _, err := os.Stat(filepath.Join(dir, storeFile)); errors.Is(err, os.ErrNotExist) {
		return &state.State{}, nil
	}
	store, err := openStore(dir, true)
	if err != nil {
		return nil, err
	}
	defer func() { _ = store.Close() }() // read only
	snapshots, err := raft.NewFileSnapshotStoreWithLogger(dir, keptSnapshots, hclog.NewNullLogger())
	if err != nil {
		return nil, fmt.Errorf("open the Raft snapshots: %w", err)
	}
	machine := newFSM()
	after, err := restoreLatest(machine, snapshots)
	if err != nil {
		return nil, err
	}
	last, err := store.LastIndex()
	if err != nil {
		return nil, fmt.Errorf("read the Raft log: %w", err)
	}
	for index := after + 1; index <= last; index++ {
		var entry raft.Log
		if err := store.GetLog(index, &entry); err != nil {
			return nil, fmt.Errorf("read Raft entry %d: %w", index, err)
		}
		if entry.Type == raft.LogCommand {
			machine.Apply(&entry)
		}
	}
	if err := machine.haltError(); err != nil {
		return nil, err
	}
	return machine.state, nil
}

// restoreLatest restores the newest snapshot into the fsm and returns its
// index; 0 without one.
func restoreLatest(machine *fsm, snapshots raft.SnapshotStore) (uint64, error) {
	listed, err := snapshots.List()
	if err != nil {
		return 0, fmt.Errorf("list the Raft snapshots: %w", err)
	}
	if len(listed) == 0 {
		return 0, nil
	}
	_, source, err := snapshots.Open(listed[0].ID)
	if err != nil {
		return 0, fmt.Errorf("open Raft snapshot %s: %w", listed[0].ID, err)
	}
	if err := machine.Restore(source); err != nil {
		return 0, fmt.Errorf("read Raft snapshot %s: %w", listed[0].ID, err)
	}
	return listed[0].Index, nil
}

// Recover makes this member the only voter of its cluster, with the state
// its log holds (raft.RecoverCluster): clowderd recover, after the other
// hosts were powered off for good. Raft writes a snapshot of that state
// with the new configuration and drops the log. The agent must not run.
func Recover(cfg Config) error {
	store, err := openStore(cfg.Dir, false)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }() // the result is in the snapshot
	snapshots, err := raft.NewFileSnapshotStoreWithLogger(cfg.Dir, keptSnapshots, hclog.NewNullLogger())
	if err != nil {
		return fmt.Errorf("open the Raft snapshots: %w", err)
	}
	config := raft.DefaultConfig()
	config.LocalID = serverID(cfg.ID)
	config.Logger = hclog.NewNullLogger()
	address, transport := raft.NewInmemTransport(raft.ServerAddress(cfg.Advertise))
	defer func() { _ = transport.Close() }() // only encodes the configuration
	configuration := raft.Configuration{Servers: []raft.Server{
		{ID: config.LocalID, Address: address, Suffrage: raft.Voter},
	}}
	machine := newFSM()
	if err := raft.RecoverCluster(config, machine, store, store, snapshots, transport, configuration); err != nil {
		return fmt.Errorf("recover Raft with this host alone: %w", errors.Join(err, machine.haltError()))
	}
	return nil
}

// openStore opens the Raft store, read only or not, and gives up after
// storeLockTimeout while another process holds it.
func openStore(dir string, readOnly bool) (*raftboltdb.BoltStore, error) {
	store, err := raftboltdb.New(raftboltdb.Options{
		Path:        filepath.Join(dir, storeFile),
		BoltOptions: &bbolt.Options{ReadOnly: readOnly, Timeout: storeLockTimeout},
	})
	if errors.Is(err, bolterrors.ErrTimeout) {
		return nil, fmt.Errorf("open the Raft store: another process holds it; stop clowderd.service first: %w", err)
	}
	if err != nil {
		return nil, fmt.Errorf("open the Raft store: %w", err)
	}
	return store, nil
}
