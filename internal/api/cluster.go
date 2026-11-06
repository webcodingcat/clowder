package api

import (
	"errors"
	"fmt"
	"net/http"

	"filippo.io/age"

	"github.com/webcodingcat/clowder/internal/secretvalue"
	"github.com/webcodingcat/clowder/internal/state"
)

// Encryption names the recipients that every value is encrypted for (GET
// /v1/encryption): the cluster's key and the recovery key. The CLI asks for
// them each time it encrypts, rather than trusting a file that someone may
// have changed.
type Encryption struct {
	Cluster  string `json:"cluster"`
	Recovery string `json:"recovery"`
}

func (s *Server) encryption(w http.ResponseWriter, _ *http.Request) {
	identity, recovery, err := s.recipients()
	if err != nil {
		writeError(w, recipientsStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, Encryption{Cluster: identity.Recipient().String(), Recovery: recovery.String()})
}

// errNoRecoveryKey is the answer of a new cluster before its first
// clowder recovery rotate. Nothing can be encrypted for the cluster yet.
var errNoRecoveryKey = errors.New("the cluster has no recovery key yet: make one with clowder recovery rotate")

// recipientsStatus returns the HTTP status for an error of recipients. A
// cluster without a recovery key is a state that the user ends, not a
// fault.
func recipientsStatus(err error) int {
	if errors.Is(err, errNoRecoveryKey) {
		return http.StatusConflict
	}
	return http.StatusInternalServerError
}

// recipients returns the cluster's age key, which decrypts the values, and
// the recovery key's recipient. Every value is encrypted for both.
func (s *Server) recipients() (*age.X25519Identity, *age.X25519Recipient, error) {
	identity, err := s.clusterIdentity()
	if err != nil {
		return nil, nil, err
	}
	var recovery string
	s.config.Cluster.Read(func(current *state.State) { recovery = current.RecoveryRecipient })
	if recovery == "" {
		return nil, nil, errNoRecoveryKey
	}
	recoveryRecipient, err := age.ParseX25519Recipient(recovery)
	if err != nil {
		return nil, nil, fmt.Errorf("the cluster's recovery key: %w", err)
	}
	return identity, recoveryRecipient, nil
}

// RekeyRequest is the body of POST /v1/encryption/rekey: values in
// secretvalue's armor, each with its place in its file.
type RekeyRequest struct {
	Values []PlacedValue `json:"values"`
}

// PlacedValue is a value and the Secret it must be for, as its file says.
// For is what secretvalue.OpenFor wants; the value's envelope must name
// the same Secret.
type PlacedValue struct {
	For       string `json:"for"`
	Encrypted string `json:"encrypted"`
}

// RekeyAnswer holds the values of a RekeyRequest encrypted again, in the
// request's order.
type RekeyAnswer struct {
	Values []string `json:"values"`
}

// rekey encrypts values again for the cluster's current recipients, each
// for the Secret it was for, so that every value can be read with the
// recovery key. A value made by hand with age may lack the recovery key,
// and after a new recovery key, all values lack it. A value that is not
// meant for its place is refused, as an apply would refuse it. The cluster
// decrypts the values and answers only with ciphertext; no value leaves it
// in clear. It changes nothing in the state, so only the log tells who
// rekeyed.
func (s *Server) rekey(w http.ResponseWriter, r *http.Request) {
	var request RekeyRequest
	if err := decodeStrict(r.Body, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	identity, recoveryRecipient, err := s.recipients()
	if err != nil {
		writeError(w, recipientsStatus(err), err)
		return
	}
	answer := RekeyAnswer{Values: make([]string, len(request.Values))}
	for index, value := range request.Values {
		opened, err := secretvalue.OpenFor(value.Encrypted, value.For, identity)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("value %d: %w", index+1, err))
			return
		}
		if answer.Values[index], err = secretvalue.Encrypt(opened, identity.Recipient(), recoveryRecipient); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	s.config.Logger.Info("values rekeyed", "actor", actorOf(identityOf(r)), "values", len(request.Values))
	writeJSON(w, http.StatusOK, answer)
}

// clusterIdentity decrypts the cluster's age key from the state. Every
// value is encrypted for this key.
func (s *Server) clusterIdentity() (*age.X25519Identity, error) {
	var sealed state.Sealed
	s.config.Cluster.Read(func(current *state.State) {
		if current.Keys != nil {
			sealed = current.Keys.AgeIdentity
		}
	})
	if sealed == nil {
		return nil, state.ErrNotBootstrapped
	}
	opened, err := s.config.Keys.Open(sealed)
	if err != nil {
		return nil, fmt.Errorf("open the cluster's age key: %w", err)
	}
	identity, err := age.ParseX25519Identity(string(opened))
	if err != nil {
		return nil, fmt.Errorf("read the cluster's age key: %w", err)
	}
	return identity, nil
}
