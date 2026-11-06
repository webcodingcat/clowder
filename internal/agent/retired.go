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
// cluster. clowderd.service does not restart it (exit status 4).
var ErrRetired = errors.New("clowderd recover took this host out of the cluster (Hetzner label " +
	"clowder-retired); set its server up anew or delete it")

// retiredCheckTimeout limits the call to Hetzner at the agent's start. It
// stays well within systemd's start timeout.
const retiredCheckTimeout = 20 * time.Second

// checkRetired asks Hetzner whether clowderd recover retired this host
// (ErrRetired). It runs before the agent starts Raft, the network or any
// stack. A host is retired when its server's label clowder-retired names
// the host's certificate. Two retired hosts would still hold the old
// cluster's majority: they would elect a leader, their copies would have
// DRBD's quorum, and they would fence the recovered host. A server that was
// set up anew has a new certificate, so the label does not stop it.
//
// The token comes from the state in the Raft log on disk, so the start
// replays the log a second time. Without a ClusterConfiguration there is no
// token, and then recover retires no host either. If Hetzner does not
// answer, the agent starts anyway. An outage at Hetzner must not keep a
// host down, and a retired host does harm only if it starts during such an
// outage together with another retired host.
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
