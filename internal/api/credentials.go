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

	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/pki"
	"github.com/webcodingcat/clowder/internal/state"
)

// Credentials come from root on a host, like kubeadm's admin.conf, and so
// do the first ones. clowderd credentials makes a key and asks its own
// agent, with the host's certificate, for a CLI certificate (POST
// /v1/internal/credentials). The CA is trusted through SSH. The cluster
// lists no CLI certificates. With RevokeOlder, the agent also commits the
// new certificate's start as the cutoff (set-cli-cutoff), and every agent
// then refuses the CLI certificates issued before it.

// CredentialsRequest asks for a CLI certificate for a public key, with a
// name for the audit (pki.DefaultCLIName without one).
type CredentialsRequest struct {
	PublicKey   []byte `json:"public_key"` // Ed25519
	Name        string `json:"name"`
	RevokeOlder bool   `json:"revoke_older"`
}

// CredentialsAnswer is the new CLI certificate and the CA to trust, with
// what the context needs: the cluster's name and the endpoints.
type CredentialsAnswer struct {
	CACertificate string `json:"ca_certificate"`
	Certificate   string `json:"certificate"`
	ClusterName   string `json:"cluster_name"`
	// Endpoints are the public addresses of the hosts that are not
	// removed, as the state knows them now.
	Endpoints []string `json:"endpoints"`
}

// internalCredentials issues a CLI certificate for root on a host, and
// sets the cutoff to the certificate's start if asked.
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
	s.config.Logger.Info("issued a CLI certificate", "host", identityOf(r).Name, "actor", state.CLIActor(name))
	answer := CredentialsAnswer{CACertificate: string(ca.CertificatePEM()), Certificate: string(certificatePEM)}
	s.config.Cluster.Read(func(current *state.State) {
		answer.ClusterName = current.ClusterName
		// without a port: the client adds the API's port (Address). A host
		// that is down now may be back later, and costs the CLI only its
		// connect timeout once per command (the client starts with the
		// endpoint that answered last), so every host that is not removed
		// counts.
		for _, id := range slices.Sorted(maps.Keys(current.Hosts)) {
			if host := current.Hosts[id]; host.Phase != state.Removed {
				answer.Endpoints = append(answer.Endpoints, host.PublicAddress.String())
			}
		}
	})
	writeJSON(w, http.StatusOK, answer)
}

// authority decrypts the cluster CA from the state with the data key.
func (s *Server) authority() (*pki.CA, error) {
	var keys *state.ClusterKeys
	s.config.Cluster.Read(func(current *state.State) {
		if current.Keys != nil {
			copied := *current.Keys
			keys = &copied
		}
	})
	if keys == nil {
		return nil, state.ErrNotBootstrapped
	}
	keyPEM, err := s.config.Keys.Decrypt(keys.CAKey)
	if err != nil {
		return nil, fmt.Errorf("open the CA key: %w", err)
	}
	return pki.LoadCA(keys.CACertificate, keyPEM)
}

// cliCutoffPassed tells whether a CLI certificate that starts at notBefore
// was issued after the cluster's cutoff. The cutoff is the one way that a
// CLI certificate ends before it expires. Certificates count in seconds, so
// a certificate issued in the same second as the new one also stays valid.
func (s *Server) cliCutoffPassed(notBefore time.Time) bool {
	var cutoff time.Time
	s.config.Cluster.Read(func(current *state.State) { cutoff = current.CLICutoff })
	return !notBefore.Before(cutoff)
}
