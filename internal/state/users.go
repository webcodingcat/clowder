package state

import (
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"
)

// revokedKept is how long a revoked certificate stays on the list after it
// expired: a host whose clock runs behind still takes it for valid.
const revokedKept = 7 * 24 * time.Hour

// AddUser records the cluster's one user, whom the bootstrap makes, with
// their first certificate. Only a cluster without a user takes it: two
// bootstraps at once cannot both get in. Further certificates come only
// through RecoverUser.
type AddUser struct {
	Name   string `json:"name"`
	Serial string `json:"serial"`
	// NotAfter is when the certificate expires; once it is revoked, it stays
	// on the revocation list until then.
	NotAfter time.Time `json:"not_after"`
	// RecoveryRecipient is the public half of the recovery key the
	// bootstrap made (State.RecoveryRecipient).
	RecoveryRecipient string `json:"recovery_recipient"`
}

func applyAddUser(s *State, _ Command, payload AddUser) (func(), error) {
	switch {
	case len(s.Users) > 0:
		return nil, errors.New("the cluster has a user already, and a cluster has only one")
	case payload.Name == "":
		return nil, errors.New("the user has no name")
	case payload.Serial == "":
		return nil, errors.New("the certificate has no serial")
	case payload.NotAfter.IsZero():
		return nil, errors.New("the certificate has no expiry")
	case s.serialInUse(payload.Serial):
		return nil, fmt.Errorf("serial %s is used already", payload.Serial)
	case !strings.HasPrefix(payload.RecoveryRecipient, "age1"):
		return nil, errors.New("the recovery key's recipient is no age key (age1...)")
	}
	return func() {
		// the key is in the user data, the OpenTofu state and the metadata
		// service; from now on it opens nothing
		s.BootstrapKey = nil
		s.RecoveryRecipient = payload.RecoveryRecipient
		s.Users[payload.Name] = &User{Name: payload.Name, Serials: map[string]time.Time{payload.Serial: payload.NotAfter}}
	}, nil
}

// serialInUse tells whether a serial belongs to a user or a host, or was
// revoked.
func (s *State) serialInUse(serial string) bool {
	if _, revoked := s.Revoked[serial]; revoked {
		return true
	}
	for _, user := range s.Users {
		if _, ok := user.Serials[serial]; ok {
			return true
		}
	}
	for _, host := range s.Hosts {
		if host.CertificateSerial == serial {
			return true
		}
	}
	return false
}

// RecoverUser gives a user who lost their credentials a new certificate,
// after a code from a host proved it (clowder user recover). It revokes
// every other certificate of the user in the same command: a stolen
// laptop is locked out with it.
type RecoverUser struct {
	Name     string    `json:"name"`
	Serial   string    `json:"serial"`
	NotAfter time.Time `json:"not_after"`
	// Code is the code that proved it, which opens no second time.
	Code UsedCode `json:"code"`
}

func applyRecoverUser(s *State, cmd Command, payload RecoverUser) (func(), error) {
	user, ok := s.Users[payload.Name]
	switch {
	case !ok:
		return nil, fmt.Errorf("no user %s", payload.Name)
	case payload.Serial == "":
		return nil, errors.New("the certificate has no serial")
	case payload.NotAfter.IsZero():
		return nil, errors.New("the certificate has no expiry")
	case s.serialInUse(payload.Serial):
		return nil, fmt.Errorf("serial %s is used already", payload.Serial)
	}
	if err := s.checkCodeUnused(payload.Code); err != nil {
		return nil, err
	}
	return func() {
		s.useCode(payload.Code, cmd.At)
		for serial, notAfter := range user.Serials {
			s.revoke(serial, notAfter, cmd.At)
		}
		user.Serials = map[string]time.Time{payload.Serial: payload.NotAfter}
	}, nil
}

// SetRecoveryRecipient puts a new recovery key in place of the old one
// (clowder recovery rotate): values encrypted from now on are for it.
type SetRecoveryRecipient struct {
	Recipient string `json:"recipient"`
	// Code is the caller's code that allowed it, which opens no second
	// time.
	Code UsedCode `json:"code"`
}

func applySetRecoveryRecipient(s *State, cmd Command, payload SetRecoveryRecipient) (func(), error) {
	switch {
	case !strings.HasPrefix(payload.Recipient, "age1"):
		return nil, errors.New("the recovery key's recipient is no age key (age1...)")
	case payload.Recipient == s.RecoveryRecipient:
		return nil, errUnchanged
	}
	if err := s.checkCodeUnused(payload.Code); err != nil {
		return nil, err
	}
	return func() {
		s.useCode(payload.Code, cmd.At)
		s.RecoveryRecipient = payload.Recipient
	}, nil
}

// UsedCode names a recovery code that a command uses
// (internal/recoverycode). It holds the code's digest rather than the code,
// so the log and the audit do not show a code that may still be valid. It
// also holds the time after which no agent accepts the code any more.
type UsedCode struct {
	Digest  string    `json:"digest"`
	Expires time.Time `json:"expires"`
}

// ErrCodeUsed refuses a recovery code that was used already. Someone who
// saw the code on a console or in a log must not be able to use it after
// its user did: that would lock the user out through their own way back
// in.
var ErrCodeUsed = errors.New("the code was used already; the next quarter of an hour brings a new one")

// usedCodeKept is how long a used code stays on the list after it
// expired: an agent whose clock runs behind the leader's still takes it.
const usedCodeKept = time.Hour

// checkCodeUnused refuses a command without its code, or with one used
// before.
func (s *State) checkCodeUnused(code UsedCode) error {
	if code.Digest == "" || code.Expires.IsZero() {
		return errors.New("the command names no recovery code")
	}
	if _, used := s.UsedCodes[code.Digest]; used {
		return ErrCodeUsed
	}
	return nil
}

// useCode records a code as used. The list grows only here, so here it
// also drops the codes that expired more than usedCodeKept before the
// command, by the command's time, so every agent drops the same.
func (s *State) useCode(code UsedCode, at time.Time) {
	maps.DeleteFunc(s.UsedCodes, func(_ string, expires time.Time) bool {
		return at.Sub(expires) > usedCodeKept
	})
	s.UsedCodes[code.Digest] = code.Expires
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
