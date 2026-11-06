// Package raftnode runs this host's member of the cluster's Raft group
// (hashicorp/raft): the log and its stable store in BoltDB, snapshots as
// files, the network over mutual TLS with host certificates, and the cluster
// state as the replicated state machine.
package raftnode

import (
	"cmp"
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
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"

	"github.com/webcodingcat/clowder/internal/state"
)

const (
	// snapshotThreshold is how many entries Raft writes before it takes a
	// snapshot. Raft then cuts the log before the snapshot.
	snapshotThreshold = 8192
	keptSnapshots     = 2
	// storeFile holds the log and the stable store.
	storeFile = "raft.db"
	// proposeTimeout bounds how long a command waits to be committed.
	proposeTimeout = 10 * time.Second
)

// Config says how this host takes part.
type Config struct {
	// Dir holds the log store and the snapshots.
	Dir string
	// ID is the host's ID, which is also its Raft ID.
	ID state.HostID
	// Bind is the address to listen on. Advertise is the address where
	// peers reach this host (the node address and port 7400).
	Bind      string
	Advertise string
	// Certificate is the host's certificate with its key. CA checks the
	// peers' certificates.
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
	// leads is set once this member leads and has applied every entry from
	// before it became leader (followLead). Only then may the leader's loops
	// act on the state.
	leads atomic.Bool
	// closed ends followLead
	closed chan struct{}
	// closeOnce and closeErr make Close safe to call twice: a rejoin closes
	// the member before the agent stops (clowderd/daemon/rejoin.go)
	closeOnce sync.Once
	closeErr  error
	logger    *slog.Logger
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
	store, err := raftboltdb.NewBoltStore(filepath.Join(cfg.Dir, storeFile))
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
		// Raft only says that it found no snapshot it could restore. The fsm
		// knows the reason: the state could not read the snapshot.
		return nil, fmt.Errorf("start Raft: %w", cmp.Or(machine.haltError(), err))
	}
	node := &Node{
		raft: member, fsm: machine, transport: transport, store: store, id: config.LocalID,
		closed: make(chan struct{}), logger: cfg.Logger,
	}
	go node.followLead()
	return node, nil
}

// leadPoll is how often followLead tries its barrier again, and how often
// a follower checks how far Raft has applied the log.
const leadPoll = 50 * time.Millisecond

// followLead sets leads while this member leads, starting once it has
// applied every entry of the log. A new leader may not yet have applied
// what its predecessor committed. Its loops and the API would then decide on
// an old state. Raft's barrier waits until every entry is applied.
func (n *Node) followLead() {
	for {
		select {
		case <-n.closed:
			return
		case leading := <-n.raft.LeaderCh():
			n.leads.Store(false)
			for leading && n.raft.State() == raft.Leader {
				err := n.raft.Barrier(proposeTimeout).Error()
				if err == nil {
					n.leads.Store(true)
					break
				}
				n.logger.Warn("apply the Raft log before leading", "error", err)
				select {
				case <-n.closed:
					return
				case <-time.After(leadPoll):
				}
			}
		}
	}
}

func serverID(id state.HostID) raft.ServerID {
	return raft.ServerID(strconv.Itoa(int(id)))
}

// Bootstrap makes this member a cluster of its own, with itself as the only
// voter (init on the first host). A member that already has a log is left
// as it is.
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

// Propose commits a command through the leader and returns its index. The
// error is either the state's refusal or the reason why the command did not
// commit: not the leader, no quorum, a timeout, or an unknown kind.
func (n *Node) Propose(ctx context.Context, cmd state.Command) (uint64, error) {
	if err := state.CheckKnown(cmd.Kind); err != nil {
		return 0, fmt.Errorf("commit %s: %w", cmd.Kind, err)
	}
	encoded, err := json.Marshal(cmd)
	if err != nil {
		return 0, fmt.Errorf("encode %s: %w", cmd.Kind, err)
	}
	timeout := proposeTimeout
	if deadline, ok := ctx.Deadline(); ok {
		timeout = min(timeout, time.Until(deadline))
	}
	// Raft treats a timeout of 0 or less as no timeout at all
	if err := ctx.Err(); err != nil || timeout <= 0 {
		return 0, fmt.Errorf("commit %s: %w", cmd.Kind, cmp.Or(err, context.DeadlineExceeded))
	}
	future := n.raft.Apply(encoded, timeout)
	if err := future.Error(); err != nil {
		return 0, fmt.Errorf("commit %s: %w", cmd.Kind, err)
	}
	if refused, ok := future.Response().(error); ok && refused != nil {
		return future.Index(), &state.Refused{Err: refused}
	}
	return future.Index(), nil
}

// CatchUp waits for a leader, because after a restart Raft replays the log
// in the background. On the leader, it also waits for followLead's barrier.
// A follower knows only what its leader has sent it so far, so before it
// acts on the state it waits for the leader's commit index (WaitCommitted).
func (n *Node) CatchUp(ctx context.Context) error {
	for {
		_, known := n.Leader()
		// a leader is caught up once followLead's barrier has gone through
		if known && (n.raft.State() != raft.Leader || n.IsLeader()) {
			return nil
		}
		select {
		case <-time.After(leadPoll):
		case <-ctx.Done():
			return fmt.Errorf("wait for a Raft leader that has applied its log: %w", ctx.Err())
		}
	}
}

// CommitIndex is the index of the last entry that this member knows to be
// committed. On the leader, it covers every entry that the cluster
// committed. Followers ask the leader for it (WaitCommitted).
func (n *Node) CommitIndex() uint64 {
	return n.raft.CommitIndex()
}

// WaitCommitted returns once the state holds every entry up to index, the
// leader's commit index. The state counts only commands (State.Index).
// Raft's own entries (a new leader's first entry, a change of members)
// never reach it. So WaitCommitted waits for Raft to apply index, and then
// for the state to hold the last command up to index.
func (n *Node) WaitCommitted(ctx context.Context, index uint64) error {
	for n.raft.AppliedIndex() < index {
		select {
		case <-time.After(leadPoll):
		case <-ctx.Done():
			return fmt.Errorf("wait for Raft to apply index %d: %w", index, ctx.Err())
		}
	}
	command, err := n.lastCommandUpTo(index)
	if err != nil {
		return err
	}
	return n.fsm.waitFor(ctx, command)
}

// lastCommandUpTo is the index of the last command at or before index. It
// is 0 when the log before index is cut, because a snapshot then holds
// that part.
func (n *Node) lastCommandUpTo(index uint64) (uint64, error) {
	first, err := n.store.FirstIndex()
	if err != nil {
		return 0, fmt.Errorf("read the Raft log: %w", err)
	}
	for at := index; at >= first && at > 0; at-- {
		var entry raft.Log
		if err := n.store.GetLog(at, &entry); err != nil {
			return 0, fmt.Errorf("read Raft entry %d: %w", at, err)
		}
		if entry.Type == raft.LogCommand {
			return at, nil
		}
	}
	return 0, nil
}

// VerifyLeader makes sure that this member still leads: a majority of the
// voters must confirm it as their leader. A leader that is cut off from the
// others believes that it leads until its lease runs out. So the leader
// calls VerifyLeader before it powers off a server or moves the Floating IP.
func (n *Node) VerifyLeader(ctx context.Context) error {
	future := n.raft.VerifyLeader()
	answered := make(chan error, 1)
	go func() { answered <- future.Error() }()
	select {
	case err := <-answered:
		if err != nil {
			return fmt.Errorf("verify the lead: %w", err)
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("verify the lead: %w", ctx.Err())
	}
}

// Read runs a function on the current state. The function must not keep
// the state or parts of it, because Apply changes them in place.
func (n *Node) Read(use func(current *state.State)) {
	n.fsm.read(use)
}

// SharedCopy returns a copy of the state to read outside the lock. Every
// caller gets the same copy until the next change. No caller may change it.
// A caller that needs to change it makes its own (State.Clone under Read).
// The tests of the loops check this (statetest.ReadOnlyCopy).
func (n *Node) SharedCopy() *state.State {
	return n.fsm.sharedCopy()
}

// WaitIndex returns once this member has applied index, so that a read
// after a write through another member sees the write.
func (n *Node) WaitIndex(ctx context.Context, index uint64) error {
	return n.fsm.waitFor(ctx, index)
}

// Changes returns what the log changed after index, oldest first, and the
// state's index. It returns state.ErrChangesGone if this member no longer
// keeps all of these changes (state.ChangeRing).
func (n *Node) Changes(since uint64) ([]state.Change, uint64, error) {
	return n.fsm.changesSince(since)
}

// Changed returns a channel that closes when the next entry is applied. The
// agent's reconcilers and the watch wait on it.
func (n *Node) Changed() <-chan struct{} {
	n.fsm.mu.RLock()
	defer n.fsm.mu.RUnlock()
	return n.fsm.changed
}

// Halted returns a channel that closes when this member meets an entry or a
// snapshot that it cannot read (state.ErrUnreadable). The member then
// applies nothing more, and the agent must stop. HaltError says which entry
// or snapshot it was.
func (n *Node) Halted() <-chan struct{} {
	return n.fsm.halted
}

// HaltError returns why the member halted, or nil if it has not halted.
func (n *Node) HaltError() error {
	return n.fsm.haltError()
}

// IsLeader tells whether this member leads and has applied the log from
// before it became leader (followLead).
func (n *Node) IsLeader() bool {
	return n.raft.State() == raft.Leader && n.leads.Load()
}

// Leader returns the leading host. ok is false while there is none.
func (n *Node) Leader() (id state.HostID, ok bool) {
	_, raftID := n.raft.LeaderWithID()
	number, err := strconv.Atoi(string(raftID))
	if err != nil {
		return 0, false
	}
	return state.HostID(number), true
}

// Close stops the member. The log and snapshots stay on disk. A second
// call returns the first one's error.
func (n *Node) Close() error {
	n.closeOnce.Do(func() {
		close(n.closed)
		shutdownErr := n.raft.Shutdown().Error()
		n.closeErr = errors.Join(shutdownErr, n.transport.Close(), n.store.Close())
	})
	return n.closeErr
}
