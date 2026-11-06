package api

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"

	"github.com/webcodingcat/clowder/internal/hostrole"
	"github.com/webcodingcat/clowder/internal/state"
)

// HostSecret is a Secret's values in clear, by key, for a host that runs a
// stack that names it. Digest is the state's digest of these values.
type HostSecret struct {
	Digest string            `json:"digest"`
	Data   map[string][]byte `json:"data"`
}

// HostCertificate is the key of a certificate in clear, for the host that
// holds the Floating IP. Chain tells which certificate of its domain the
// key belongs to.
type HostCertificate struct {
	Chain []byte `json:"chain"`
	Key   []byte `json:"key"`
}

// errNoKeys answers a host that asks a host without the data key. Only a
// control-plane host holds it, and only such a host serves other hosts.
var errNoKeys = &StatusError{
	Status:  http.StatusServiceUnavailable,
	Message: "this host holds no data key; only " + string(hostrole.ControlPlane) + " hosts do",
}

// hostSecrets serves GET /v1/internal/hosts/self/secrets: the secrets that
// the caller may read, and nothing else.
func (s *Server) hostSecrets(w http.ResponseWriter, r *http.Request) {
	answer, err := s.secretsOf(state.HostID(identityOf(r).HostID))
	if err != nil {
		s.writeFailure(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, answer)
}

// hostCertificates serves GET /v1/internal/hosts/self/tls: the certificates'
// keys, by domain, for the host that holds the Floating IP.
func (s *Server) hostCertificates(w http.ResponseWriter, r *http.Request) {
	answer, err := s.certificatesOf(state.HostID(identityOf(r).HostID))
	if err != nil {
		s.writeFailure(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, answer)
}

// writeFailure answers a StatusError with its own status (errNoKeys: 503)
// and any other error with 500, which it also logs, because only the
// caller would see it otherwise.
func (s *Server) writeFailure(w http.ResponseWriter, r *http.Request, err error) {
	var statusError *StatusError
	if errors.As(err, &statusError) {
		writeError(w, statusError.Status, errors.New(statusError.Message))
		return
	}
	s.config.Logger.Error("answer a call", "path", r.URL.Path, "caller", identityOf(r).Name, "error", err)
	writeError(w, http.StatusInternalServerError, err)
}

// secretsOf opens the secrets that host may read (state.SecretsOfHost), as
// Kubernetes' node authorizer gives a node only the secrets of its pods.
func (s *Server) secretsOf(host state.HostID) (map[string]HostSecret, error) {
	if s.config.Keys == nil {
		return nil, errNoKeys
	}
	readable := map[string]state.EncryptedSecret{}
	s.config.Cluster.Read(func(current *state.State) {
		for id := range current.SecretsOfHost(host) {
			if secret, ok := current.Secrets[id]; ok {
				// a copy is safe: Apply replaces a secret's values, it never
				// changes them
				readable[id] = secret.Spec
			}
		}
	})
	opened := map[string]HostSecret{}
	for _, id := range slices.Sorted(maps.Keys(readable)) {
		secret := HostSecret{Digest: readable[id].Digest, Data: map[string][]byte{}}
		for _, key := range readable[id].Keys() {
			value, err := s.config.Keys.Decrypt(readable[id].Data[key])
			if err != nil {
				return nil, fmt.Errorf("open secret %s for host %d: %w", id, host, err)
			}
			secret.Data[key] = value
		}
		opened[id] = secret
	}
	return opened, nil
}

// certificatesOf opens the certificates' keys for host while the state
// says that it holds the Floating IP. The host that takes over the Floating
// IP gets the keys from the control plane. The leader moves the Floating IP
// and is on the control plane, so it can always give them.
func (s *Server) certificatesOf(host state.HostID) (map[string]HostCertificate, error) {
	if s.config.Keys == nil {
		return nil, errNoKeys
	}
	var holds bool
	var certificates []state.Certificate
	s.config.Cluster.Read(func(current *state.State) {
		if holds = current.Ingress != nil && *current.Ingress == host; !holds {
			return
		}
		for _, domain := range slices.Sorted(maps.Keys(current.Certificates)) {
			// a copy is safe: Apply replaces a certificate, it never changes one
			certificates = append(certificates, *current.Certificates[domain])
		}
	})
	if !holds {
		return nil, &StatusError{
			Status:  http.StatusForbidden,
			Message: fmt.Sprintf("host %d does not hold the Floating IP, so it gets no certificate's key", host),
		}
	}
	opened := map[string]HostCertificate{}
	for _, certificate := range certificates {
		key, err := s.config.Keys.Decrypt(certificate.Key)
		if err != nil {
			return nil, fmt.Errorf("open the key of the certificate of %s: %w", certificate.Domain, err)
		}
		opened[certificate.Domain] = HostCertificate{Chain: certificate.Chain, Key: key}
	}
	return opened, nil
}

// SecretsOfThisHost returns the secrets that this host may read, by ID
// (state.SecretsOfHost). A control-plane host opens them itself; any other
// host asks the leader.
func (s *Server) SecretsOfThisHost(ctx context.Context) (map[string]HostSecret, error) {
	if s.config.Keys != nil {
		return s.secretsOf(s.config.HostID)
	}
	var answer map[string]HostSecret
	if err := s.askForThisHost(ctx, "/v1/internal/hosts/self/secrets", &answer); err != nil {
		return nil, fmt.Errorf("ask the control plane for this host's secrets: %w", err)
	}
	return answer, nil
}

// CertificatesOfThisHost returns the certificates' keys by domain, while
// this host holds the Floating IP. A control-plane host opens them itself;
// any other host asks the leader.
func (s *Server) CertificatesOfThisHost(ctx context.Context) (map[string]HostCertificate, error) {
	if s.config.Keys != nil {
		return s.certificatesOf(s.config.HostID)
	}
	var answer map[string]HostCertificate
	if err := s.askForThisHost(ctx, "/v1/internal/hosts/self/tls", &answer); err != nil {
		return nil, fmt.Errorf("ask the control plane for the certificates' keys: %w", err)
	}
	return answer, nil
}

// askForThisHost asks the leader, which is a control-plane host. The leader
// waits until its state holds this host's (MinIndexHeader), so that it
// does not judge from an older state what this host may read.
func (s *Server) askForThisHost(ctx context.Context, path string, answer any) error {
	id, here, err := s.waitForLeader(ctx)
	switch {
	case err != nil:
		return err
	case here:
		// a host without the data key never votes, so it never leads
		return errors.New("this host leads but holds no data key; its roles and key files disagree")
	}
	var index uint64
	s.config.Cluster.Read(func(current *state.State) { index = current.Index })
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+s.config.APIAddress(id)+path, nil)
	if err != nil {
		return err
	}
	request.Header.Set(VersionHeader, s.config.Version)
	request.Header.Set(MinIndexHeader, strconv.FormatUint(index, 10))
	if err := send(s.forwarder, request, answer); err != nil {
		return fmt.Errorf("the leader (host %d): %w", id, err)
	}
	return nil
}
