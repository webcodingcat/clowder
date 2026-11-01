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

// AddUser records a certificate the leader issued to a user; a new name
// creates the user.
type AddUser struct {
	Name   string `json:"name"`
	Serial string `json:"serial"`
	// NotAfter is when the certificate expires; a revocation is kept until
	// then.
	NotAfter time.Time `json:"not_after"`
	// First is the bootstrap's user, who may only come into a cluster
	// without users: two bootstraps at once cannot both get in.
	First bool `json:"first,omitempty"`
	// RecoveryRecipient comes with the first user: the public half of the
	// recovery key the bootstrap made (State.RecoveryRecipient).
	RecoveryRecipient string `json:"recovery_recipient,omitempty"`
}

func applyAddUser(s *State, _ Command, payload AddUser) (func(), error) {
	switch {
	case payload.Name == "":
		return nil, errors.New("the user has no name")
	case payload.Serial == "":
		return nil, errors.New("the certificate has no serial")
	case payload.NotAfter.IsZero():
		return nil, errors.New("the certificate has no expiry")
	case s.serialInUse(payload.Serial):
		return nil, fmt.Errorf("serial %s is used already", payload.Serial)
	case payload.First && len(s.Users) > 0:
		return nil, errors.New("the cluster has its first user already; users add others")
	case payload.First != strings.HasPrefix(payload.RecoveryRecipient, "age1"):
		return nil, errors.New("the first user, and only the first, brings the recovery key's recipient (age1...)")
	}
	return func() {
		if payload.First {
			s.RecoveryRecipient = payload.RecoveryRecipient
		}
		user, ok := s.Users[payload.Name]
		if !ok {
			user = &User{Name: payload.Name, Serials: map[string]time.Time{}}
			s.Users[payload.Name] = user
		}
		user.Serials[payload.Serial] = payload.NotAfter
	}, nil
}

// serialInUse tells whether a serial belongs to a user or was revoked.
func (s *State) serialInUse(serial string) bool {
	if _, revoked := s.Revoked[serial]; revoked {
		return true
	}
	for _, user := range s.Users {
		if _, ok := user.Serials[serial]; ok {
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
	return func() {
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

// RemoveUser removes a user and revokes all their certificates.
type RemoveUser struct {
	Name string `json:"name"`
}

func applyRemoveUser(s *State, cmd Command, payload RemoveUser) (func(), error) {
	user, ok := s.Users[payload.Name]
	if !ok {
		return nil, fmt.Errorf("no user %s", payload.Name)
	}
	return func() {
		for serial, notAfter := range user.Serials {
			s.revoke(serial, notAfter, cmd.At)
		}
		delete(s.Users, payload.Name)
	}, nil
}

// Revoke puts one certificate of a user or a host on the revocation list:
// a lost laptop, or a host that is gone. The state knows when the
// certificate expires, so it refuses a serial it has not recorded.
type Revoke struct {
	Serial string `json:"serial"`
}

func applyRevoke(s *State, cmd Command, payload Revoke) (func(), error) {
	if payload.Serial == "" {
		return nil, errors.New("the serial is empty")
	}
	if _, revoked := s.Revoked[payload.Serial]; revoked {
		return nil, fmt.Errorf("serial %s is revoked already", payload.Serial)
	}
	notAfter, ok := s.certificateExpiry(payload.Serial)
	if !ok {
		return nil, fmt.Errorf("no user or host has the certificate with serial %s", payload.Serial)
	}
	return func() {
		s.revoke(payload.Serial, notAfter, cmd.At)
		for _, user := range s.Users {
			delete(user.Serials, payload.Serial)
		}
	}, nil
}

// certificateExpiry tells when the certificate of a user or a host expires.
func (s *State) certificateExpiry(serial string) (time.Time, bool) {
	for _, user := range s.Users {
		if notAfter, ok := user.Serials[serial]; ok {
			return notAfter, true
		}
	}
	for _, host := range s.Hosts {
		if host.CertificateSerial == serial {
			return host.CertificateNotAfter, true
		}
	}
	return time.Time{}, false
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
