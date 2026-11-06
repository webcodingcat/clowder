package state

import (
	"errors"
	"maps"
	"strings"
	"time"
)

const cliActorPrefix = "cli/"

// CLIActor returns the CLI's actor with the name in its certificate. The
// name is for the audit and grants nothing.
func CLIActor(name string) string {
	return cliActorPrefix + name
}

// IsCLIActor tells whether an actor is the CLI, whatever its name. pki
// checks the name when it issues the certificate.
func IsCLIActor(actor string) bool {
	return strings.HasPrefix(actor, cliActorPrefix) && len(actor) > len(cliActorPrefix)
}

// SetCLICutoff refuses every CLI certificate issued before Cutoff
// (clowderd credentials --revoke-older). A host commits it, because root on
// that host issued the new certificate.
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
// has expired, because a host whose clock runs behind still treats it as
// valid.
const revokedKept = 7 * 24 * time.Hour

// revoke puts a certificate on the revocation list. The list grows only
// here, so revoke also drops the certificates that expired more than
// revokedKept before the command's time.
func (s *State) revoke(serial string, notAfter, at time.Time) {
	maps.DeleteFunc(s.Revoked, func(_ string, revocation Revocation) bool {
		return at.Sub(revocation.NotAfter) > revokedKept
	})
	if _, revoked := s.Revoked[serial]; !revoked {
		s.Revoked[serial] = Revocation{At: at, NotAfter: notAfter}
	}
}
