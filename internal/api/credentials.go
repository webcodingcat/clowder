package api

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"net/http"
	"time"

	"filippo.io/age"

	"github.com/webcodingcat/clowder/internal/pki"
	"github.com/webcodingcat/clowder/internal/state"
)

// Credentials come from root on a host, as kubeadm's admin.conf does:
// clowderd credentials makes a key and asks its own agent, with the host's
// certificate, for a CLI certificate (POST /v1/internal/credentials). The
// cluster lists no CLI certificates; with RevokeOlder, the agent also
// commits the new certificate's start as the cutoff (set-cli-cutoff), and
// every agent refuses the CLI certificates issued before it.

// CredentialsRequest asks for a CLI certificate for a public key.
type CredentialsRequest struct {
	PublicKey   []byte `json:"public_key"` // Ed25519
	RevokeOlder bool   `json:"revoke_older"`
}

// CredentialsAnswer is the new CLI certificate and the CA to trust.
type CredentialsAnswer struct {
	CACertificate string `json:"ca_certificate"`
	Certificate   string `json:"certificate"`
}

// internalCredentials issues a CLI certificate for root on a host, and
// sets the cutoff to it if asked.
func (s *Server) internalCredentials(w http.ResponseWriter, r *http.Request) {
	var request CredentialsRequest
	if err := decodeStrict(r.Body, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if len(request.PublicKey) != ed25519.PublicKeySize {
		writeError(w, http.StatusBadRequest, errors.New("the public key is no Ed25519 key"))
		return
	}
	ca, err := s.authority()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	certificatePEM, err := ca.IssueCLI(ed25519.PublicKey(request.PublicKey), time.Now())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if request.RevokeOlder {
		certificate, err := pki.ParseCertificate(certificatePEM)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		// no time here: the leader stamps its own (proposeHere)
		cmd, err := state.NewCommand(state.KindSetCLICutoff, time.Time{}, actorOf(identityOf(r)),
			state.SetCLICutoff{Cutoff: certificate.NotBefore})
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		index, ok := s.commit(w, r, cmd, s.Propose)
		if !ok {
			return
		}
		// a follower answers once it refuses the old certificates itself;
		// the other agents follow within moments
		ctx, cancel := context.WithTimeout(r.Context(), waitTimeout)
		defer cancel()
		if err := s.config.Cluster.WaitIndex(ctx, index); err != nil {
			writeError(w, http.StatusGatewayTimeout, fmt.Errorf("the cutoff is committed, but this agent "+
				"has not applied it yet: %w", err))
			return
		}
		s.config.Logger.Warn("CLI certificates issued before the new one are refused from now on",
			"host", identityOf(r).Name, "cutoff", certificate.NotBefore)
	}
	s.config.Logger.Info("issued a CLI certificate", "host", identityOf(r).Name)
	writeJSON(w, http.StatusOK, CredentialsAnswer{
		CACertificate: string(ca.CertificatePEM()), Certificate: string(certificatePEM),
	})
}

// cliCutoffPassed tells whether a CLI certificate that starts at notBefore
// was issued after the cluster's cutoff, the one way a CLI certificate
// ends before it expires. Certificates count in seconds, so one issued in
// the same second as the new one stays valid too.
func (s *Server) cliCutoffPassed(notBefore time.Time) bool {
	var cutoff time.Time
	s.config.Cluster.Read(func(current *state.State) { cutoff = current.CLICutoff })
	return !notBefore.Before(cutoff)
}

// RotateRequest asks for a new recovery key.
type RotateRequest struct {
	Recipient string `json:"recipient"`
}

// rotateRecovery puts a new recovery key in place. The CLI's certificate is
// enough: whoever holds it controls the cluster anyway.
func (s *Server) rotateRecovery(w http.ResponseWriter, r *http.Request) {
	var request RotateRequest
	if err := decodeStrict(r.Body, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if _, err := age.ParseX25519Recipient(request.Recipient); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("the recovery key's recipient: %w", err))
		return
	}
	s.commitRequest(w, r, state.KindSetRecoveryRecipient, state.SetRecoveryRecipient{Recipient: request.Recipient})
}
