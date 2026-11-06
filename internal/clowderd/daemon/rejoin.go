package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/webcodingcat/clowder/internal/atomicfile"
	"github.com/webcodingcat/clowder/internal/clowderd/hostfiles"
	"github.com/webcodingcat/clowder/internal/state"
	"github.com/webcodingcat/clowder/internal/storage"
)

// ErrRejoining ends the agent after a rejoin, with an exit status of its
// own that clowderd.service counts as a success (SuccessExitStatus) and
// restarts. The start then sets the Raft log aside (setRaftAsideToRejoin).
var ErrRejoining = errors.New("clowderd recover on a control-plane host kept this host: starting again with a " +
	"fresh Raft log, to follow the new leader")

// rejoiner is what a worker does for clowderd recover on a control-plane
// host that keeps it (api.RecoverHost; docs/developing/cluster.md,
// Recover). The worker's Raft log may hold a higher term, or entries that
// the old majority committed without the recovering host. The new leader
// would never send them, and a higher term would end its lead. A
// non-voter's log is only a copy, so the worker sets it aside and follows
// the new leader from its snapshot.
type rejoiner struct {
	// Storage gives api.RecoverHost its DiskStates, and Rejoin its
	// DropPeers (storage.Driver)
	Storage
	files hostfiles.Files
	// closeRaft stops this host's Raft member (raftnode.Node.Close)
	closeRaft func() error
	logger    *slog.Logger
	// rejoined closes once a rejoin is asked for; the agent then ends
	// (watch)
	rejoined chan struct{}
	once     sync.Once
}

func newRejoiner(base shared) *rejoiner {
	return &rejoiner{
		Storage: base.parts.Storage, files: base.files, closeRaft: base.node.Close, logger: base.logger,
		rejoined: make(chan struct{}),
	}
}

// Rejoin writes the marker only after the peers are dropped, so a failed
// drop leaves the Raft log as it is and recover can ask again. It stops
// the Raft member before it answers: recover then starts the new leader,
// which must never reach the old member and its term.
func (r *rejoiner) Rejoin(ctx context.Context, retired []state.HostID) error {
	if err := r.DropPeers(ctx, storage.Peers(retired)); err != nil {
		return err
	}
	if err := atomicfile.Write(r.files.RejoinMarker(), []byte("set the Raft log aside at the next start\n"), 0o600); err != nil {
		return fmt.Errorf("mark the rejoin: %w", err)
	}
	r.logger.Warn("clowderd recover keeps this host; the agent starts again to follow the new leader",
		"retired", retired)
	r.once.Do(func() { close(r.rejoined) })
	if err := r.closeRaft(); err != nil {
		// the agent ends anyway, and the marker sets the log aside
		r.logger.Warn("stop the Raft member to rejoin", "error", err)
	}
	return nil
}

// setRaftAsideToRejoin moves the Raft directory aside when a rejoin asked
// for it. The old log stays for a human to look at; nothing reads it again.
// A crash before the marker is gone moves the new, empty directory aside
// too, which does no harm.
func setRaftAsideToRejoin(files hostfiles.Files, now time.Time, logger *slog.Logger) error {
	if _, err := os.Stat(files.RejoinMarker()); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("look for a rejoin: %w", err)
	}
	aside := files.RaftDir() + ".set-aside-" + now.UTC().Format("20060102T150405Z")
	if err := os.Rename(files.RaftDir(), aside); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("set the Raft log aside to rejoin: %w", err)
	}
	if err := os.Remove(files.RejoinMarker()); err != nil {
		return fmt.Errorf("set the Raft log aside to rejoin: %w", err)
	}
	logger.Warn("the Raft log is set aside; this host follows the new leader as a fresh member", "aside", aside)
	return nil
}
