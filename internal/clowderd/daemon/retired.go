package daemon

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/webcodingcat/clowder/internal/clowderd/hostfiles"
	"github.com/webcodingcat/clowder/internal/controller"
	"github.com/webcodingcat/clowder/internal/hostrole"
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

// checkRetired returns ErrRetired when the server's label clowder-retired
// names this host's certificate: two retired hosts would still hold the old
// cluster's majority. A server set up anew has a new certificate, so the
// label does not stop it. Without a ClusterConfiguration there is no token,
// and then recover retires no host either.
//
// Only a control-plane host checks, as only it holds the token: a retired
// voter is the danger. A retired worker never votes, and no control-plane
// host accepts its revoked certificate.
//
// If Hetzner does not answer, the agent starts anyway. An outage at Hetzner
// must not keep a host down, and a retired host does harm only if it starts
// during such an outage together with another retired host.
func checkRetired(ctx context.Context, current *state.State, host hostfiles.Host, credentials hostfiles.Credentials,
	logger *slog.Logger,
) error {
	if !current.Configured() || !host.Roles.Has(hostrole.ControlPlane) {
		return nil
	}
	client, err := controller.OpenHetzner(current, credentials.Decrypt)
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
	case serial != "" && serial == pki.Serial(credentials.Certificate.Leaf):
		return ErrRetired
	}
	return nil
}
