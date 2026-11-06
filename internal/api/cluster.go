package api

import (
	"fmt"
	"net/http"

	"filippo.io/age"

	"github.com/webcodingcat/clowder/internal/state"
)

// PublicSealingKey is the public half of the cluster's sealing key (GET
// /v1/sealing-key), which clowder secret seal seals for. The CLI asks for
// it each time it seals, rather than trusting a copy that someone may have
// changed.
type PublicSealingKey struct {
	Recipient string `json:"recipient"`
}

func (s *Server) publicSealingKey(w http.ResponseWriter, _ *http.Request) {
	identity, err := s.sealingKey()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, PublicSealingKey{Recipient: identity.Recipient().String()})
}

// SealingKeyAnswer is the answer of /v1/internal/sealing-key: the
// cluster's sealing key in age's text form.
type SealingKeyAnswer struct {
	Identity string `json:"identity"`
}

// internalSealingKey gives the sealing key to root on this host, which
// asks with the host's certificate (clowderd sealing-key export), as
// clowderd credentials does for the CLI's certificate. The host holds the
// data key that opens the sealing key in the state anyway, so the route
// gives it nothing new; it only saves root from reading the state. The log
// tells which host exported it.
func (s *Server) internalSealingKey(w http.ResponseWriter, r *http.Request) {
	identity, err := s.sealingKey()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.config.Logger.Warn("the sealing key was exported", "host", identityOf(r).Name)
	writeJSON(w, http.StatusOK, SealingKeyAnswer{Identity: identity.String()})
}

// sealingKey decrypts the cluster's sealing key from the state with the
// data key. Every value of a SealedSecret is sealed for this key.
func (s *Server) sealingKey() (*age.X25519Identity, error) {
	var encrypted state.Encrypted
	s.config.Cluster.Read(func(current *state.State) {
		if current.Keys != nil {
			encrypted = current.Keys.SealingKey
		}
	})
	if encrypted == nil {
		return nil, state.ErrNotBootstrapped
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
