package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"filippo.io/age"

	"github.com/webcodingcat/clowder/internal/digest"
	"github.com/webcodingcat/clowder/internal/pki"
	"github.com/webcodingcat/clowder/internal/state"
)

// The user's way in: the cluster has no user yet, so nobody has a
// certificate. Whoever holds the bootstrap key (OpenTofu made it, cloud-init
// gave it to init) proves it with HMACs instead. The key opens only this
// once: the bootstrap's add-user drops it from the state.
//
//  1. GET /v1/bootstrap: the agent sends its CA certificate and, while the
//     cluster has no user, pki.TrustProof over it made with the bootstrap
//     key. The CLI trusts the CA only if the proof matches its key. The
//     offer also tells whether the cluster has its user already, so the
//     CLI stops before it makes a recovery key that would never count.
//  2. POST /v1/bootstrap over TLS checked against that CA: the user's name
//     and public key and the recovery key's recipient, signed with the
//     bootstrap key. The agent issues the user's first certificate; after
//     that, the endpoint refuses.

// BootstrapOffer is the agent's answer to the first contact. It comes
// unchecked: the proof makes the CA trustworthy, and Bootstrapped tells
// anyone whether the cluster has a user, but not which. Once it has one,
// there is no proof.
type BootstrapOffer struct {
	CACertificate string `json:"ca_certificate"`
	Proof         []byte `json:"proof,omitempty"`
	Bootstrapped  bool   `json:"bootstrapped"`
}

// BootstrapRequest asks for the user's first certificate.
type BootstrapRequest struct {
	User      string `json:"user"`
	PublicKey []byte `json:"public_key"` // Ed25519
	// RecoveryRecipient is the public half of the recovery key the CLI made.
	RecoveryRecipient string    `json:"recovery_recipient"`
	Time              time.Time `json:"time"`
	MAC               []byte    `json:"mac"`
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

// mac is an HMAC over the request's fields, each after its length, so no
// other fields give it the same input (a line end in the user's name
// cannot pass for the end of a field).
func (r *BootstrapRequest) mac(bootstrapKey []byte) []byte {
	mac := hmac.New(sha256.New, bootstrapKey)
	digest.WriteParts(mac, []byte("clowder bootstrap"), []byte(r.User), r.PublicKey,
		[]byte(r.RecoveryRecipient), []byte(r.Time.UTC().Format(time.RFC3339)))
	return mac.Sum(nil)
}

// BootstrapAnswer is the user's first certificate.
type BootstrapAnswer struct {
	Certificate string `json:"certificate"`
}

// TrustedCluster is a cluster the CLI found at endpoint and trusts: its CA
// proved it knows the bootstrap key.
type TrustedCluster struct {
	endpoint, version string
	connectTimeout    time.Duration
	bootstrapKey      []byte
	CA                *x509.Certificate
	CAPEM             string
}

// ErrBootstrapped is FindCluster's answer when the cluster has its user
// already: the bootstrap key opens nothing then, so the agent proves
// nothing with it either.
var ErrBootstrapped = errors.New("the cluster has a user already, and a cluster has only one")

// FindCluster asks the agent at endpoint for its CA and trusts it only if it
// proves it knows the bootstrap key.
func FindCluster(ctx context.Context, endpoint string, connectTimeout time.Duration, bootstrapKey []byte,
	version string,
) (*TrustedCluster, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+Address(endpoint)+"/v1/bootstrap", nil)
	if err != nil {
		return nil, fmt.Errorf("ask %s for its CA: %w", endpoint, err)
	}
	request.Header.Set(VersionHeader, version)
	var offer BootstrapOffer
	if err := send(uncheckedClient(connectTimeout), request, &offer); err != nil {
		return nil, fmt.Errorf("ask %s for its CA: %w", endpoint, err)
	}
	// unchecked, but harmless: it stops a bootstrap, the user's own
	if offer.Bootstrapped {
		return nil, ErrBootstrapped
	}
	if !pki.CheckTrustProof(bootstrapKey, []byte(offer.CACertificate), offer.Proof) {
		return nil, fmt.Errorf("%s cannot prove it knows the bootstrap key: wrong key, or not your cluster", endpoint)
	}
	ca, err := pki.ParseCertificate([]byte(offer.CACertificate))
	if err != nil {
		return nil, fmt.Errorf("read the CA of %s: %w", endpoint, err)
	}
	return &TrustedCluster{
		endpoint: endpoint, version: version, connectTimeout: connectTimeout, bootstrapKey: bootstrapKey,
		CA: ca, CAPEM: offer.CACertificate,
	}, nil
}

// Bootstrap gets the user's first certificate (PEM), over TLS checked
// against the trusted CA, and brings the recovery key's recipient.
func (c *TrustedCluster) Bootstrap(ctx context.Context, user string, public ed25519.PublicKey,
	recoveryRecipient string,
) (string, error) {
	signed := BootstrapRequest{User: user, PublicKey: public, RecoveryRecipient: recoveryRecipient, Time: time.Now().UTC()}
	signed.Sign(c.bootstrapKey)
	body, err := json.Marshal(signed)
	if err != nil {
		return "", fmt.Errorf("encode the request: %w", err)
	}
	url := "https://" + Address(c.endpoint) + "/v1/bootstrap"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("get the first certificate from %s: %w", c.endpoint, err)
	}
	request.Header.Set(VersionHeader, c.version)
	var answer BootstrapAnswer
	if err := send(checkedClient(c.CA, c.connectTimeout), request, &answer); err != nil {
		return "", fmt.Errorf("get the first certificate from %s: %w", c.endpoint, err)
	}
	return answer.Certificate, nil
}

// uncheckedClient is for a first contact without a certificate (bootstrap,
// join, recovery): there is nothing to check the agent against yet, so a
// trust proof in its answer decides.
func uncheckedClient(connectTimeout time.Duration) *http.Client {
	tlsConfig := &tls.Config{
		InsecureSkipVerify: true, // the caller checks the trust proof
		MinVersion:         tls.VersionTLS13,
	}
	return &http.Client{
		Timeout:   answerTimeout,
		Transport: endpointTransport(tlsConfig, connectTimeout),
	}
}

// checkedClient talks without a certificate to agents of a CA a trust
// proof made trustworthy.
func checkedClient(ca *x509.Certificate, connectTimeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:   answerTimeout,
		Transport: endpointTransport(pki.ClientTLSToHosts(tls.Certificate{}, ca, nil), connectTimeout),
	}
}

// bootstrapOffer answers the first contact.
func (s *Server) bootstrapOffer(w http.ResponseWriter, _ *http.Request) {
	var caPEM []byte
	var bootstrapped bool
	s.config.Cluster.Read(func(current *state.State) {
		if current.Keys != nil {
			caPEM = slices.Clone(current.Keys.CACertificate)
		}
		bootstrapped = len(current.Users) > 0
	})
	if caPEM == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("the cluster is not bootstrapped yet"))
		return
	}
	offer := BootstrapOffer{CACertificate: string(caPEM), Bootstrapped: bootstrapped}
	// after the bootstrap no proof at all: it would let anyone try keys
	// offline
	key, err := s.bootstrapKey()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if key != nil {
		offer.Proof = pki.TrustProof(key, caPEM)
	}
	writeJSON(w, http.StatusOK, offer)
}

// bootstrap issues the user's first certificate.
func (s *Server) bootstrap(w http.ResponseWriter, r *http.Request) {
	// not strict: a CLI of the next release may send a field this agent does
	// not know, and the MAC covers every field the bootstrap reads
	var request BootstrapRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("read the request: %w", err))
		return
	}
	// first, before any key is opened: after the bootstrap, every request
	// here ends at once. The state decides (AddUser); this spares the
	// work. The offer tells anyone as much.
	var hasUsers bool
	s.config.Cluster.Read(func(current *state.State) { hasUsers = len(current.Users) > 0 })
	if hasUsers {
		writeError(w, http.StatusConflict, errors.New("the cluster has a user already, and a cluster has only one"))
		return
	}
	if age := time.Since(request.Time); age > bootstrapMaxAge || age < -bootstrapMaxAge {
		writeError(w, http.StatusUnauthorized, errors.New("the request is too old (or a clock is off by minutes)"))
		return
	}
	key, err := s.bootstrapKey()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if key == nil || !request.SignedWith(key) {
		writeError(w, http.StatusUnauthorized, errors.New("the request is not signed with the bootstrap key"))
		return
	}
	if err := pki.CheckUserName(request.User); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if len(request.PublicKey) != ed25519.PublicKeySize {
		writeError(w, http.StatusBadRequest, errors.New("the public key is no Ed25519 key"))
		return
	}
	if _, err := age.ParseX25519Recipient(request.RecoveryRecipient); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("the recovery key's recipient: %w", err))
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
	// no time here: the leader stamps its own (proposeHere)
	cmd, err := state.NewCommand(state.KindAddUser, time.Time{}, state.UserActor(request.User),
		state.AddUser{
			Name: request.User, Serial: pki.Serial(certificate), NotAfter: certificate.NotAfter,
			RecoveryRecipient: request.RecoveryRecipient,
		})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if _, ok := s.commit(w, r, cmd, s.Propose); ok {
		writeJSON(w, http.StatusOK, BootstrapAnswer{Certificate: string(certificatePEM)})
	}
}

// bootstrapKey opens the bootstrap key; nil once the cluster has its user.
func (s *Server) bootstrapKey() ([]byte, error) {
	var sealed state.Sealed
	s.config.Cluster.Read(func(current *state.State) {
		if len(current.Users) == 0 {
			sealed = slices.Clone(current.BootstrapKey)
		}
	})
	if sealed == nil {
		return nil, nil
	}
	key, err := s.config.Keys.Open(sealed)
	if err != nil {
		return nil, fmt.Errorf("open the bootstrap key: %w", err)
	}
	return key, nil
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
