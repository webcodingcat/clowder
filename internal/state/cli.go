package state

import (
	"errors"
	"maps"
	"strings"
	"time"
)

// cliActorPrefix starts the actor of every CLI certificate.
const cliActorPrefix = "cli/"

// CLIActor is the CLI with the name in its certificate, for the audit. A
// cluster has no users: the name grants nothing, and whoever holds a CLI
// certificate controls the cluster, as with kubeadm's admin.conf.
func CLIActor(name string) string {
	return cliActorPrefix + name
}

// IsCLIActor tells whether an actor is the CLI, whatever its name. The
// name grants nothing, so nothing here checks it; pki checks it when it
// issues the certificate.
func IsCLIActor(actor string) bool {
	return strings.HasPrefix(actor, cliActorPrefix) && len(actor) > len(cliActorPrefix)
}

// SetCLICutoff refuses every CLI certificate issued before Cutoff
// (clowderd credentials --revoke-older). This is how to lock out a lost
// laptop. A host commits the command, because root on that host issued the
// new certificate.
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

// revokedKept is how long a revoked certificate stays on the list after it
// has expired: a host whose clock runs behind still treats it as valid.
const revokedKept = 7 * 24 * time.Hour

// revoke puts a certificate on the revocation list. The list grows only
// here, so this is also where it drops the certificates that expired more
// than revokedKept before the command; no handshake accepts them any more.
// It uses the command's time, never the clock, so every agent drops the
// same certificates.
func (s *State) revoke(serial string, notAfter, at time.Time) {
	maps.DeleteFunc(s.Revoked, func(_ string, revocation Revocation) bool {
		return at.Sub(revocation.NotAfter) > revokedKept
	})
	if _, revoked := s.Revoked[serial]; !revoked {
		s.Revoked[serial] = Revocation{At: at, NotAfter: notAfter}
	}
}
