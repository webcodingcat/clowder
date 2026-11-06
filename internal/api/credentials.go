package api

import (
	"cmp"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/webcodingcat/clowder/internal/hostrole"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/pki"
	"github.com/webcodingcat/clowder/internal/state"
)

// CredentialsRequest asks for a CLI certificate for a public key. Name is
// for the audit; without it, the name is pki.DefaultCLIName.
type CredentialsRequest struct {
	PublicKey   []byte `json:"public_key"` // Ed25519
	Name        string `json:"name"`
	RevokeOlder bool   `json:"revoke_older"`
}

// CredentialsAnswer holds the new CLI certificate, the CA to trust, and
// the rest of what a context needs: the cluster's name and the endpoints.
type CredentialsAnswer struct {
	CACertificate string `json:"ca_certificate"`
	Certificate   string `json:"certificate"`
	ClusterName   string `json:"cluster_name"`
	// Endpoints are the control-plane hosts as the state knows them now.
	Endpoints []string `json:"endpoints"`
}

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
		s.writeFailure(w, r, err)
		return
	}
	name := cmp.Or(request.Name, pki.DefaultCLIName)
	if !names.Valid(name) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("CLI name %q: %s", name, names.Rule))
		return
	}
	certificatePEM, err := ca.IssueCLI(ed25519.PublicKey(request.PublicKey), name, time.Now())
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
		// no time here: the leader stamps its own clock (proposeHere)
		cmd, err := state.NewCommand(state.KindSetCLICutoff, time.Time{}, actorOf(identityOf(r)),
			state.SetCLICutoff{Cutoff: certificate.NotBefore})
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		index, ok := s.commit(w, r, cmd, s.config.Peers.Propose)
		if !ok {
			return
		}
		// answer only once this agent refuses the old certificates itself;
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
	s.config.Logger.Info("issued a CLI certificate", "host", identityOf(r).Name, "actor", state.CLIActor(name))
	answer := CredentialsAnswer{CACertificate: string(ca.CertificatePEM()), Certificate: string(certificatePEM)}
	s.config.Cluster.Read(func(current *state.State) {
		answer.ClusterName = current.ClusterName
		// without a port: the client adds the API's port (Address). A host
		// that is down now may be back later, and costs the CLI only one
		// connect timeout per command until then.
		for _, id := range slices.Sorted(maps.Keys(current.Hosts)) {
			if host := current.Hosts[id]; host.Phase != state.Removed && host.Roles.Has(hostrole.ControlPlane) {
				answer.Endpoints = append(answer.Endpoints, host.PublicAddress.String())
			}
		}
	})
	writeJSON(w, http.StatusOK, answer)
}

func (s *Server) authority() (*pki.CA, error) {
	var keys *state.ClusterKeys
	s.config.Cluster.Read(func(current *state.State) {
		if current.Keys != nil {
			copied := *current.Keys
			keys = &copied
		}
	})
	switch {
	case keys == nil:
		return nil, state.ErrNotBootstrapped
	case s.config.Keys == nil:
		return nil, errNoKeys
	}
	keyPEM, err := s.config.Keys.Decrypt(keys.CAKey)
	if err != nil {
		return nil, fmt.Errorf("open the CA key: %w", err)
	}
	return pki.LoadCA(keys.CACertificate, keyPEM)
}

// cliCutoffPassed keeps a certificate issued in the same second as the
// cutoff valid, because certificate times count in seconds.
func (s *Server) cliCutoffPassed(notBefore time.Time) bool {
	var cutoff time.Time
	s.config.Cluster.Read(func(current *state.State) { cutoff = current.CLICutoff })
	return !notBefore.Before(cutoff)
}
