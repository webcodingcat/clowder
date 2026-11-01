package state

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// AddUser records a certificate the leader issued to a user; a new name
// creates the user.
type AddUser struct {
	Name   string `json:"name"`
	Serial string `json:"serial"`
	// First is the bootstrap's user, who may only come into a cluster
	// without users: two bootstraps at once cannot both get in.
	First bool `json:"first,omitempty"`
	// RecoveryRecipient comes with the first user: the public half of the
	// recovery key the bootstrap made (State.RecoveryRecipient).
	RecoveryRecipient string `json:"recovery_recipient,omitempty"`
}

func applyAddUser(s *State, _ Command, payload AddUser) error {
	switch {
	case payload.Name == "":
		return errors.New("the user has no name")
	case payload.Serial == "":
		return errors.New("the certificate has no serial")
	case s.serialInUse(payload.Serial):
		return fmt.Errorf("serial %s is used already", payload.Serial)
	case payload.First && len(s.Users) > 0:
		return errors.New("the cluster has its first user already; users add others")
	case payload.First != strings.HasPrefix(payload.RecoveryRecipient, "age1"):
		return errors.New("the first user, and only the first, brings the recovery key's recipient (age1...)")
	}
	if payload.First {
		s.RecoveryRecipient = payload.RecoveryRecipient
	}
	user, ok := s.Users[payload.Name]
	if !ok {
		user = &User{Name: payload.Name}
		s.Users[payload.Name] = user
	}
	user.Serials = append(user.Serials, payload.Serial)
	return nil
}

// serialInUse tells whether a serial belongs to a user or was revoked.
func (s *State) serialInUse(serial string) bool {
	if _, revoked := s.Revoked[serial]; revoked {
		return true
	}
	for _, user := range s.Users {
		if slices.Contains(user.Serials, serial) {
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
	Name   string `json:"name"`
	Serial string `json:"serial"`
}

func applyRecoverUser(s *State, cmd Command, payload RecoverUser) error {
	user, ok := s.Users[payload.Name]
	switch {
	case !ok:
		return fmt.Errorf("no user %s", payload.Name)
	case payload.Serial == "":
		return errors.New("the certificate has no serial")
	case s.serialInUse(payload.Serial):
		return fmt.Errorf("serial %s is used already", payload.Serial)
	}
	for _, serial := range user.Serials {
		s.Revoked[serial] = cmd.At
	}
	user.Serials = []string{payload.Serial}
	return nil
}

// SetRecoveryRecipient puts a new recovery key in place of the old one
// (clowder recovery rotate): values encrypted from now on are for it.
type SetRecoveryRecipient struct {
	Recipient string `json:"recipient"`
}

func applySetRecoveryRecipient(s *State, _ Command, payload SetRecoveryRecipient) error {
	switch {
	case !strings.HasPrefix(payload.Recipient, "age1"):
		return errors.New("the recovery key's recipient is no age key (age1...)")
	case payload.Recipient == s.RecoveryRecipient:
		return errUnchanged
	}
	s.RecoveryRecipient = payload.Recipient
	return nil
}

// RemoveUser removes a user and revokes all their certificates.
type RemoveUser struct {
	Name string `json:"name"`
}

func applyRemoveUser(s *State, cmd Command, payload RemoveUser) error {
	user, ok := s.Users[payload.Name]
	if !ok {
		return fmt.Errorf("no user %s", payload.Name)
	}
	for _, serial := range user.Serials {
		s.Revoked[serial] = cmd.At
	}
	delete(s.Users, payload.Name)
	return nil
}

// Revoke puts one certificate on the revocation list: a lost laptop, or a
// host that is gone.
type Revoke struct {
	Serial string `json:"serial"`
}

func applyRevoke(s *State, cmd Command, payload Revoke) error {
	if payload.Serial == "" {
		return errors.New("the serial is empty")
	}
	if _, revoked := s.Revoked[payload.Serial]; revoked {
		return fmt.Errorf("serial %s is revoked already", payload.Serial)
	}
	s.Revoked[payload.Serial] = cmd.At
	for _, user := range s.Users {
		user.Serials = slices.DeleteFunc(user.Serials, func(serial string) bool { return serial == payload.Serial })
	}
	return nil
}
