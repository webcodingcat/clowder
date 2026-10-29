// Package raftnode runs this host's member of the cluster's Raft group
// (hashicorp/raft): the log and its stable store in BoltDB, snapshots as
// files, the network over mutual TLS with host certificates, and the cluster
// state as the replicated state machine.
package raftnode

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"

	"github.com/webcodingcat/clowder/internal/state"
)

const (
	// snapshotThreshold is how many entries Raft writes before a snapshot;
	// the log before it is cut.
	snapshotThreshold = 8192
	// keptSnapshots is how many snapshots stay on disk.
	keptSnapshots = 2
	// proposeTimeout bounds how long a command waits to be committed.
	proposeTimeout = 10 * time.Second
)

// Config says how this host takes part.
type Config struct {
	// Dir holds the log store and the snapshots.
	Dir string
	// ID is the host's ID, which is also its Raft ID.
	ID state.HostID
	// Bind is where to listen; Advertise is how peers reach it (the node
	// address and port 7400).
	Bind      string
	Advertise string
	// Certificate is the host's certificate with its key; CA checks the peers'.
	Certificate tls.Certificate
	CA          *x509.Certificate
	Logger      *slog.Logger
}

// Node is this host's Raft member and its copy of the cluster state.
type Node struct {
	raft      *raft.Raft
	fsm       *fsm
	transport *raft.NetworkTransport
	store     *raftboltdb.BoltStore
	id        raft.ServerID
}

// Open starts the member with what is on disk. A new member does nothing
// until it is bootstrapped or a leader adds it.
func Open(cfg Config) (*Node, error) {
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("open Raft: %w", err)
	}
	advertise, err := net.ResolveTCPAddr("tcp", cfg.Advertise)
	if err != nil {
		return nil, fmt.Errorf("open Raft: advertise address: %w", err)
	}
	logger := hclog.FromStandardLogger(slog.NewLogLogger(cfg.Logger.Handler(), slog.LevelInfo),
		&hclog.LoggerOptions{Name: "raft", Level: hclog.Info})

	machine := newFSM()
	stream, err := newTLSStream(cfg.Bind, advertise, cfg.Certificate, cfg.CA, machine.revoked)
	if err != nil {
		return nil, err
	}
	transport := raft.NewNetworkTransportWithLogger(stream, 3, 10*time.Second, logger)
	store, err := raftboltdb.NewBoltStore(filepath.Join(cfg.Dir, "raft.db"))
	if err != nil {
		_ = transport.Close() // the store's error is the one to report
		return nil, fmt.Errorf("open the Raft store: %w", err)
	}
	snapshots, err := raft.NewFileSnapshotStoreWithLogger(cfg.Dir, keptSnapshots, logger)
	if err != nil {
		_ = transport.Close()
		_ = store.Close()
		return nil, fmt.Errorf("open the Raft snapshots: %w", err)
	}

	config := raft.DefaultConfig()
	config.LocalID = serverID(cfg.ID)
	config.SnapshotThreshold = snapshotThreshold
	config.Logger = logger
	member, err := raft.NewRaft(config, machine, store, store, snapshots, transport)
	if err != nil {
		_ = transport.Close()
		_ = store.Close()
		return nil, fmt.Errorf("start Raft: %w", err)
	}
	return &Node{raft: member, fsm: machine, transport: transport, store: store, id: config.LocalID}, nil
}

func serverID(id state.HostID) raft.ServerID {
	return raft.ServerID(strconv.Itoa(int(id)))
}

// Bootstrap makes this member a cluster of its own, the only voter (init on
// the first host). A member that has a log already is left as it is.
func (n *Node) Bootstrap() error {
	configuration := raft.Configuration{Servers: []raft.Server{
		{ID: n.id, Address: n.transport.LocalAddr(), Suffrage: raft.Voter},
	}}
	err := n.raft.BootstrapCluster(configuration).Error()
	if err != nil && !errors.Is(err, raft.ErrCantBootstrap) {
		return fmt.Errorf("bootstrap Raft: %w", err)
	}
	return nil
}

// AddMember adds a host to the Raft group, as a voter or not. Only the leader
// can (join, and the promotions when the third host arrives).
func (n *Node) AddMember(id state.HostID, address string, voter bool) error {
	add := n.raft.AddNonvoter
	if voter {
		add = n.raft.AddVoter
	}
	if err := add(serverID(id), raft.ServerAddress(address), 0, proposeTimeout).Error(); err != nil {
		return fmt.Errorf("add host %d to Raft: %w", id, err)
	}
	return nil
}

// Propose commits a command through the leader and returns its index. The
// error is the state's refusal or why the command did not commit (not the
// leader, no quorum, a timeout).
func (n *Node) Propose(ctx context.Context, cmd state.Command) (uint64, error) {
	encoded, err := json.Marshal(cmd)
	if err != nil {
		return 0, fmt.Errorf("encode %s: %w", cmd.Kind, err)
	}
	timeout := proposeTimeout
	if deadline, ok := ctx.Deadline(); ok {
		timeout = min(timeout, time.Until(deadline))
	}
	future := n.raft.Apply(encoded, timeout)
	if err := future.Error(); err != nil {
		return 0, fmt.Errorf("commit %s: %w", cmd.Kind, err)
	}
	if refused, ok := future.Response().(error); ok && refused != nil {
		return future.Index(), refused
	}
	return future.Index(), nil
}

// Read runs a function on the current state. It must not keep the state or
// parts of it: Apply changes them in place.
func (n *Node) Read(use func(current *state.State)) {
	n.fsm.read(use)
}

// WaitIndex returns once this member has applied index: a read after a
// write through another member sees the write.
func (n *Node) WaitIndex(ctx context.Context, index uint64) error {
	return n.fsm.waitFor(ctx, index)
}

// IsLeader tells whether this member leads.
func (n *Node) IsLeader() bool {
	return n.raft.State() == raft.Leader
}

// Leader returns the leader's address and ID, empty while there is none.
func (n *Node) Leader() (string, string) {
	address, id := n.raft.LeaderWithID()
	return string(address), string(id)
}

// Close stops the member; the log and snapshots stay on disk.
func (n *Node) Close() error {
	shutdownErr := n.raft.Shutdown().Error()
	return errors.Join(shutdownErr, n.transport.Close(), n.store.Close())
}
