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
var ErrRetired = errors.New("clowderd recover took this host out of the cluster (hoster label " +
	"clowder-retired); set its server up anew or delete it")

// retiredCheckTimeout bounds the start's question to the hoster, well within
// systemd's start timeout.
const retiredCheckTimeout = 20 * time.Second

// checkRetired asks the hoster, before the agent starts Raft, the network or
// a stack, whether clowderd recover retired this host (ErrRetired): its
// server's label clowder-retired names this host's certificate. Two retired
// hosts would still have the old cluster's majority between them: they
// would elect a leader, their copies would have DRBD's quorum, and they
// would fence the host that recovered. A server set up anew has a
// certificate of its own, so the label stops nothing there.
//
// The token comes from the state the Raft log on disk holds, as Raft does
// not run yet; reading it costs the start a second replay of the log, which
// the member does again when it opens. A host without a server ref or a
// hoster cannot be asked, and recover retires no such host. A hoster that
// does not answer lets the agent start as before: a host must not stay down
// for the hoster's outage, and a retired host only does harm if it is
// switched on while the hoster's API is down, and another one with it.
func checkRetired(ctx context.Context, files Files, host Host, credentials credentials,
	connect controller.ConnectHoster, logger *slog.Logger,
) error {
	if host.ServerRef == "" {
		return nil
	}
	current, err := raftnode.ReadState(files.raftDir())
	if err != nil {
		return fmt.Errorf("read the state before the start: %w", err)
	}
	if current.Config.Spec.Hoster.Type == "" {
		return nil
	}
	client, err := controller.OpenHoster(current, credentials.dataKey.Open, connect)
	if err != nil {
		logger.Warn("start without asking the hoster whether this host is retired", "error", err)
		return nil
	}
	asking, cancel := context.WithTimeout(ctx, retiredCheckTimeout)
	defer cancel()
	serial, err := client.RetiredSerial(asking, host.ServerRef)
	switch {
	case err != nil:
		logger.Warn("start without asking the hoster whether this host is retired", "error", err)
		return nil
	case serial != "" && serial == pki.Serial(credentials.certificate.Leaf):
		return ErrRetired
	}
	return nil
}
