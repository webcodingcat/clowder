package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/webcodingcat/clowder/internal/controller"
	"github.com/webcodingcat/clowder/internal/pki"
	"github.com/webcodingcat/clowder/internal/raftnode"
)

// ErrRetired ends the agent of a host that clowderd recover took out of the
// cluster. clowderd.service does not start it again (exit status 4).
var ErrRetired = errors.New("clowderd recover took this host out of the cluster (Hetzner label " +
	"clowder-retired); set its server up anew or delete it")

// retiredCheckTimeout bounds the start's question to Hetzner, well within
// systemd's start timeout.
const retiredCheckTimeout = 20 * time.Second

// checkRetired asks Hetzner, before the agent starts Raft, the network or a
// stack, whether clowderd recover retired this host (ErrRetired): its
// server's label clowder-retired names this host's certificate. Two retired
// hosts would still have the old cluster's majority between them: they
// would elect a leader, their copies would have DRBD's quorum, and they
// would fence the host that recovered. A server set up anew has a
// certificate of its own, so the label stops nothing there.
//
// The token comes from the state the Raft log on disk holds, as Raft does
// not run yet; reading it costs the start a second replay of the log, which
// the member does again when it opens. Before the first cluster apply
// there is no token to ask with, and recover retires no host then. An API
// that does not answer lets the agent start as before: a host must not stay
// down for Hetzner's outage, and a retired host only does harm if it is
// switched on while the API is down, and another one with it.
func checkRetired(ctx context.Context, files Files, host Host, credentials credentials, logger *slog.Logger) error {
	current, err := raftnode.ReadState(files.raftDir())
	if err != nil {
		return fmt.Errorf("read the state before the start: %w", err)
	}
	if current.Config.Revision == 0 {
		return nil
	}
	client, err := controller.OpenHetzner(current, credentials.dataKey.Open)
	if err != nil {
		logger.Warn("start without asking Hetzner whether this host is retired", "error", err)
		return nil
	}
	asking, cancel := context.WithTimeout(ctx, retiredCheckTimeout)
	defer cancel()
	serial, err := client.RetiredSerial(asking, host.ServerID)
	switch {
	case err != nil:
		logger.Warn("start without asking Hetzner whether this host is retired", "error", err)
		return nil
	case serial != "" && serial == pki.Serial(credentials.certificate.Leaf):
		return ErrRetired
	}
	return nil
}
