package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/webcodingcat/clowder/internal/pki"
	"github.com/webcodingcat/clowder/internal/state"
)

// The first user's way in: the cluster has no user yet, so nobody has a
// certificate. Whoever holds the bootstrap key (OpenTofu made it, cloud-init
// gave it to init) proves it with HMACs instead.
//
//  1. GET /v1/bootstrap: the agent sends its CA certificate and, for every
//     valid bootstrap key, pki.TrustProof over it. The CLI trusts the CA only
//     if one proof matches its key.
//  2. POST /v1/bootstrap over TLS checked against that CA: the user's name
//     and public key, signed with the bootstrap key. The agent issues the
//     first user certificate; after that, the endpoint refuses.

// BootstrapOffer is the agent's answer to the first contact.
type BootstrapOffer struct {
	CACertificate string   `json:"ca_certificate"`
	Proofs        [][]byte `json:"proofs"`
}

// BootstrapRequest asks for the first user's certificate.
type BootstrapRequest struct {
	User      string    `json:"user"`
	PublicKey []byte    `json:"public_key"` // Ed25519
	Time      time.Time `json:"time"`
	MAC       []byte    `json:"mac"`
}

// bootstrapMaxAge is how old a request may be; a replay later is refused.
const bootstrapMaxAge = 5 * time.Minute

// Sign fills in the MAC with the bootstrap key.
func (r *BootstrapRequest) Sign(bootstrapKey []byte) {
	r.MAC = r.mac(bootstrapKey)
}

// SignedWith tells whether the MAC was made with the bootstrap key.
func (r *BootstrapRequest) SignedWith(bootstrapKey []byte) bool {
	return hmac.Equal(r.mac(bootstrapKey), r.MAC)
}

func (r *BootstrapRequest) mac(bootstrapKey []byte) []byte {
	mac := hmac.New(sha256.New, bootstrapKey)
	mac.Write([]byte("clowder bootstrap\n" + r.User + "\n" +
		base64.StdEncoding.EncodeToString(r.PublicKey) + "\n" + r.Time.UTC().Format(time.RFC3339)))
	return mac.Sum(nil)
}

// BootstrapAnswer is the first user's certificate.
type BootstrapAnswer struct {
	Certificate string `json:"certificate"`
}

// Bootstrap gets the first user's certificate from the agent at seed. It
// returns the CA and the certificate, both in PEM.
func Bootstrap(ctx context.Context, seed string, bootstrapKey []byte, user string, public ed25519.PublicKey,
	version string,
) (caPEM, certificatePEM string, err error) {
	// first contact: nothing to check the agent against yet; the proof does it
	unchecked := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{
		InsecureSkipVerify: true, // the trust proof below decides
		MinVersion:         tls.VersionTLS13,
	}}}
	url := "https://" + Address(seed) + "/v1/bootstrap"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", "", err
	}
	request.Header.Set(VersionHeader, version)
	var offer BootstrapOffer
	if err := send(unchecked, request, &offer); err != nil {
		return "", "", fmt.Errorf("ask %s for its CA: %w", seed, err)
	}
	if !slices.ContainsFunc(offer.Proofs, func(proof []byte) bool {
		return pki.CheckTrustProof(bootstrapKey, []byte(offer.CACertificate), proof)
	}) {
		return "", "", fmt.Errorf("%s cannot prove it knows the bootstrap key: wrong key, or not your cluster", seed)
	}
	ca, err := pki.ParseCertificate([]byte(offer.CACertificate))
	if err != nil {
		return "", "", err
	}

	// from now on only a host of this CA is believed
	checked := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		TLSClientConfig: pki.ClientTLSToHosts(tls.Certificate{}, ca, nil),
	}}
	signed := BootstrapRequest{User: user, PublicKey: public, Time: time.Now().UTC()}
	signed.Sign(bootstrapKey)
	body, err := json.Marshal(signed)
	if err != nil {
		return "", "", err
	}
	request, err = http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	request.Header.Set(VersionHeader, version)
	var answer BootstrapAnswer
	if err := send(checked, request, &answer); err != nil {
		return "", "", fmt.Errorf("get the first certificate from %s: %w", seed, err)
	}
	return offer.CACertificate, answer.Certificate, nil
}

// bootstrapOffer answers the first contact.
func (s *Server) bootstrapOffer(w http.ResponseWriter, _ *http.Request) {
	var caPEM []byte
	s.config.Cluster.Read(func(current *state.State) {
		if current.Keys != nil {
			caPEM = slices.Clone(current.Keys.CACertificate)
		}
	})
	if caPEM == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("the cluster is not bootstrapped yet"))
		return
	}
	keys, err := s.joinKeys()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	offer := BootstrapOffer{CACertificate: string(caPEM)}
	for _, key := range keys {
		offer.Proofs = append(offer.Proofs, pki.TrustProof(key, caPEM))
	}
	writeJSON(w, http.StatusOK, offer)
}

// bootstrap issues the first user's certificate.
func (s *Server) bootstrap(w http.ResponseWriter, r *http.Request) {
	var request BootstrapRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("read the request: %w", err))
		return
	}
	if age := time.Since(request.Time); age > bootstrapMaxAge || age < -bootstrapMaxAge {
		writeError(w, http.StatusUnauthorized, errors.New("the request is too old (or a clock is off by minutes)"))
		return
	}
	keys, err := s.joinKeys()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !slices.ContainsFunc(keys, request.SignedWith) {
		writeError(w, http.StatusUnauthorized, errors.New("the request is not signed with a valid bootstrap key"))
		return
	}
	if err := pki.CheckUserName(request.User); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	// the state decides (AddUser.First); this only spares issuing a certificate
	var hasUsers bool
	s.config.Cluster.Read(func(current *state.State) { hasUsers = len(current.Users) > 0 })
	if hasUsers {
		writeError(w, http.StatusConflict, errors.New("the cluster has its first user already; users add others"))
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
	certificatePEM, err := ca.IssueUser(ed25519.PublicKey(request.PublicKey), request.User, time.Now())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	certificate, err := pki.ParseCertificate(certificatePEM)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	cmd, err := state.NewCommand(state.KindAddUser, time.Now(), state.UserActor(request.User),
		state.AddUser{Name: request.User, Serial: pki.Serial(certificate), First: true})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if _, ok := s.commit(w, r, cmd, s.propose); ok {
		writeJSON(w, http.StatusOK, BootstrapAnswer{Certificate: string(certificatePEM)})
	}
}

// joinKeys opens the valid bootstrap keys.
func (s *Server) joinKeys() ([][]byte, error) {
	var sealed []state.Sealed
	s.config.Cluster.Read(func(current *state.State) { sealed = slices.Clone(current.JoinKeys) })
	var keys [][]byte
	for _, key := range sealed {
		opened, err := s.config.Keys.Open(key)
		if err != nil {
			return nil, fmt.Errorf("open a bootstrap key: %w", err)
		}
		keys = append(keys, opened)
	}
	return keys, nil
}

// authority opens the cluster CA from the state with the data key.
func (s *Server) authority() (*pki.CA, error) {
	var keys *state.ClusterKeys
	s.config.Cluster.Read(func(current *state.State) {
		if current.Keys != nil {
			copied := *current.Keys
			keys = &copied
		}
	})
	if keys == nil {
		return nil, errors.New("the cluster is not bootstrapped yet")
	}
	keyPEM, err := s.config.Keys.Open(keys.CAKey)
	if err != nil {
		return nil, fmt.Errorf("open the CA key: %w", err)
	}
	return pki.LoadCA(keys.CACertificate, keyPEM)
}
