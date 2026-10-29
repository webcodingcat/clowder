package state

import (
	"errors"
	"fmt"
	"slices"
)

// AddUser records a certificate the leader issued to a user; a new name
// creates the user.
type AddUser struct {
	Name   string `json:"name"`
	Serial string `json:"serial"`
	// First is the bootstrap's user, who may only come into a cluster
	// without users: two bootstraps at once cannot both get in.
	First bool `json:"first,omitempty"`
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
