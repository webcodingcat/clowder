package agent

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/webcodingcat/clowder/internal/controller"
	"github.com/webcodingcat/clowder/internal/pki"
	"github.com/webcodingcat/clowder/internal/state"
)

// ErrRetired ends the agent of a host that clowderd recover took out of the
// cluster. clowderd.service does not start it again (exit status 4).
var ErrRetired = errors.New("clowderd recover took this host out of the cluster (Hetzner label " +
	"clowder-retired); set its server up anew or delete it")

// retiredCheckTimeout bounds the question to Hetzner at the agent's start.
// It is well within systemd's start timeout.
const retiredCheckTimeout = 20 * time.Second

// checkRetired asks Hetzner whether clowderd recover retired this host
// (ErrRetired). It asks before the agent starts Raft, the network or a
// stack. A host is retired when its server's label clowder-retired names
// this host's certificate. Two retired hosts would still have the old
// cluster's majority between them: they would elect a leader, their copies
// would have DRBD's quorum, and they would fence the host that recovered.
// A server that is set up again has a new certificate, so the label does
// not stop it.
//
// Raft does not run yet, so the token comes from the state in the Raft log
// on disk. Reading it costs the start a second replay of the log, because
// the Raft member replays it again when it opens. Before the first
// ClusterConfiguration there is no token to ask with, and recover retires
// no host then. If the API does not answer, the agent starts as before: a
// host must not stay down because of an outage at Hetzner, and a retired
// host only does harm if it is switched on while the API is down, together
// with another retired host.
func checkRetired(ctx context.Context, current *state.State, host Host, credentials credentials, logger *slog.Logger) error {
	if !current.Configured() {
		return nil
	}
	client, err := controller.OpenHetzner(current, credentials.dataKey.Decrypt)
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
