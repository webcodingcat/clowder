package api

import (
	"fmt"
	"net/http"

	"filippo.io/age"

	"github.com/webcodingcat/clowder/internal/state"
)

// PublicSealingKey is the public half of the cluster's sealing key (GET
// /v1/sealing-key).
type PublicSealingKey struct {
	Recipient string `json:"recipient"`
}

func (s *Server) publicSealingKey(w http.ResponseWriter, r *http.Request) {
	identity, err := s.sealingKey()
	if err != nil {
		s.writeFailure(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, PublicSealingKey{Recipient: identity.Recipient().String()})
}

// SealingKeyAnswer holds the cluster's sealing key in age's text form.
type SealingKeyAnswer struct {
	Identity string `json:"identity"`
}

// internalSealingKey gives root on this host nothing new: a control-plane
// host already holds the data key that opens the sealing key in the state,
// and any other host has no data key to open it with.
func (s *Server) internalSealingKey(w http.ResponseWriter, r *http.Request) {
	identity, err := s.sealingKey()
	if err != nil {
		s.writeFailure(w, r, err)
		return
	}
	s.config.Logger.Warn("the sealing key was exported", "host", identityOf(r).Name)
	writeJSON(w, http.StatusOK, SealingKeyAnswer{Identity: identity.String()})
}

func (s *Server) sealingKey() (*age.X25519Identity, error) {
	var encrypted state.Encrypted
	s.config.Cluster.Read(func(current *state.State) {
		if current.Keys != nil {
			encrypted = current.Keys.SealingKey
		}
	})
	switch {
	case encrypted == nil:
		return nil, state.ErrNotBootstrapped
	case s.config.Keys == nil:
		return nil, errNoKeys
	}
	opened, err := s.config.Keys.Decrypt(encrypted)
	if err != nil {
		return nil, fmt.Errorf("decrypt the cluster's sealing key: %w", err)
	}
	identity, err := age.ParseX25519Identity(string(opened))
	if err != nil {
		return nil, fmt.Errorf("read the cluster's sealing key: %w", err)
	}
	return identity, nil
}
