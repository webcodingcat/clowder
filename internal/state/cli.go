package state

import (
	"errors"
	"maps"
	"strings"
	"time"
)

// CLIActor is the CLI. A cluster has no users: whoever holds a CLI
// certificate and cluster.yaml controls it, as with kubeadm's admin.conf.
const CLIActor = "cli"

// revokedKept is how long a revoked certificate stays on the list after it
// expired: a host whose clock runs behind still takes it for valid.
const revokedKept = 7 * 24 * time.Hour

// FinishBootstrap ends the bootstrap: the CLI has its first certificate,
// and the bootstrap key opens nothing from now on. Only a cluster in its
// bootstrap takes it, so two bootstraps at once cannot both get in.
type FinishBootstrap struct {
	// RecoveryRecipient is the public half of the recovery key the
	// bootstrap made (State.RecoveryRecipient).
	RecoveryRecipient string `json:"recovery_recipient"`
}

func applyFinishBootstrap(s *State, _ Command, payload FinishBootstrap) (func(), error) {
	switch {
	case !s.InBootstrap():
		return nil, errors.New("the cluster is bootstrapped already")
	case !strings.HasPrefix(payload.RecoveryRecipient, "age1"):
		return nil, errors.New("the recovery key's recipient is no age key (age1...)")
	}
	return func() {
		// the key is in the user data, the OpenTofu state and the metadata
		// service; from now on it opens nothing
		s.BootstrapKey = nil
		s.RecoveryRecipient = payload.RecoveryRecipient
	}, nil
}

// InBootstrap tells whether the cluster waits for its CLI: it has its first
// host, and the bootstrap key still opens.
func (s *State) InBootstrap() bool {
	return s.Keys != nil && len(s.BootstrapKey) > 0
}

// SetCLICutoff refuses every CLI certificate issued before Cutoff
// (clowderd credentials --revoke-older): the way to lock out a lost
// laptop. A host commits it, as root on it issued the new certificate.
type SetCLICutoff struct {
	Cutoff time.Time `json:"cutoff"`
}

func applySetCLICutoff(s *State, cmd Command, payload SetCLICutoff) (func(), error) {
	switch {
	case !IsHostActor(cmd.Actor):
		return nil, errors.New("only a host sets the cutoff of the CLI certificates")
	case payload.Cutoff.IsZero():
		return nil, errors.New("the cutoff has no time")
	case !payload.Cutoff.After(s.CLICutoff):
		// a cutoff never moves back: that would let a revoked laptop in again
		return nil, errors.New("the cutoff is not later than the one in place")
	}
	return func() { s.CLICutoff = payload.Cutoff }, nil
}

// SetRecoveryRecipient puts a new recovery key in place of the old one
// (clowder recovery rotate): values encrypted from now on are for it.
type SetRecoveryRecipient struct {
	Recipient string `json:"recipient"`
}

func applySetRecoveryRecipient(s *State, _ Command, payload SetRecoveryRecipient) (func(), error) {
	switch {
	case !strings.HasPrefix(payload.Recipient, "age1"):
		return nil, errors.New("the recovery key's recipient is no age key (age1...)")
	case payload.Recipient == s.RecoveryRecipient:
		return nil, errUnchanged
	}
	return func() { s.RecoveryRecipient = payload.Recipient }, nil
}

// serialInUse tells whether a serial belongs to a host, or was revoked.
func (s *State) serialInUse(serial string) bool {
	if _, revoked := s.Revoked[serial]; revoked {
		return true
	}
	for _, host := range s.Hosts {
		if host.CertificateSerial == serial {
			return true
		}
	}
	return false
}

// revoke puts a certificate on the revocation list. The list grows only
// here, so here it also drops the certificates that expired more than
// revokedKept before the command: no handshake takes them any more. It
// goes by the command's time, never the clock, so every agent drops the
// same.
func (s *State) revoke(serial string, notAfter, at time.Time) {
	maps.DeleteFunc(s.Revoked, func(_ string, revocation Revocation) bool {
		return at.Sub(revocation.NotAfter) > revokedKept
	})
	if _, revoked := s.Revoked[serial]; !revoked {
		s.Revoked[serial] = Revocation{At: at, NotAfter: notAfter}
	}
}
