package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"time"

	"github.com/webcodingcat/clowder/internal/hostrole"
	"github.com/webcodingcat/clowder/internal/jointoken"
	"github.com/webcodingcat/clowder/internal/pki"
	"github.com/webcodingcat/clowder/internal/state"
)

// A host joins with a one-time token that a user made (clowder host
// join-token, internal/jointoken):
//
//  1. GET /v1/join?token=<id>: the agent sends its CA certificate and,
//     while the token is open, a pki.TrustProof over it, made with the
//     token's hash (jointoken.ProofKey). The joining host trusts the CA only
//     if the proof matches its token.
//  2. POST /v1/join over TLS checked against that CA: the token itself, the
//     host's name, addresses, server ID and public keys, a nonce and the
//     time. Any agent accepts the request and hands it to the leader. The
//     leader checks the token and the server with Hetzner, chooses the
//     host's ID, issues its certificate and commits add-host, which uses
//     the token up. The answer carries the certificate, the data key and
//     the other hosts.
//
// Neither request has a client certificate, so each agent accepts only a
// limited number of tries per minute (limiter).

// JoinOffer is the agent's answer to a joining host's first contact. It
// arrives unchecked; the proof makes the CA trustworthy.
type JoinOffer struct {
	CACertificate string `json:"ca_certificate"`
	Proof         []byte `json:"proof"`
}

// JoinRequest asks the leader to add a host.
type JoinRequest struct {
	// Token is the whole join token. It travels only over TLS checked
	// against the CA that the proof made trustworthy.
	Token         string     `json:"token"`
	Name          string     `json:"name"`
	Underlay      netip.Addr `json:"underlay"`
	PublicAddress netip.Addr `json:"public_address"`
	ServerID      int64      `json:"server_id"`
	WGPublicKey   string     `json:"wg_public_key"`
	// Roles are the roles the host takes, within what the token allows.
	Roles hostrole.Set `json:"roles"`
	// PublicKey is the host's own key (Ed25519), which its certificate is
	// issued for. The private half never leaves the host.
	PublicKey []byte `json:"public_key"`
	// Nonce and Time make each request unique. The leader accepts a
	// request only once, and only within joinMaxAge.
	Nonce []byte    `json:"nonce"`
	Time  time.Time `json:"time"`
	// Source is the address that the request came from. The agent that
	// accepted the request sets it, and the leader accepts a join only from
	// the host's own addresses.
	Source netip.Addr `json:"source,omitzero"`
}

// joinMaxAge is how old a join request may be.
const joinMaxAge = 5 * time.Minute

// JoinAnswer is what a joined host needs to start.
type JoinAnswer struct {
	Host        state.HostID `json:"host"`
	Certificate string       `json:"certificate"`
	// DataKey is the cluster's data key in age's text form.
	DataKey string `json:"data_key"`
	// Peers are the other hosts. The new host builds its WireGuard
	// interfaces to them before Raft can tell it anything.
	Peers []JoinPeer `json:"peers"`
}

// JoinPeer is another host, as the joining host's WireGuard needs it.
type JoinPeer struct {
	ID          state.HostID `json:"id"`
	WGPublicKey string       `json:"wg_public_key"`
	Underlay    netip.Addr   `json:"underlay"`
}

// Joins is the leader's side of a join, which the agent implements. It
// checks the request against the state and Hetzner, and commits it.
type Joins interface {
	// Join adds the host. A refusal is a *StatusError that the joining host
	// is shown (JoinRefused); any other error is the cluster's fault.
	Join(ctx context.Context, request JoinRequest) (JoinAnswer, error)
}

// JoinRefused is a refusal of a join, with the reason.
func JoinRefused(err error) error {
	return &StatusError{Status: http.StatusForbidden, Message: err.Error()}
}

// DefaultJoinTokenTTL is how long a join token stays open unless the user
// says otherwise.
const DefaultJoinTokenTTL = state.DefaultJoinTokenTTL

// JoinTokenRequest asks for a join token that lives for TTL, a Go
// duration ("1h", "90m"), and lets a host take the roles in Roles.
type JoinTokenRequest struct {
	TTL   string       `json:"ttl"`
	Roles hostrole.Set `json:"roles"`
}

// JoinTokenAnswer is a new join token. This is the only time the token is
// shown.
type JoinTokenAnswer struct {
	Token   string    `json:"token"`
	Expires time.Time `json:"expires"`
}

// JoiningCluster is a cluster that a joining host found and trusts,
// because its CA proved that it knows the token.
type JoiningCluster struct {
	endpoint, version string
	connectTimeout    time.Duration
	token             jointoken.Token
	CA                *x509.Certificate
	CAPEM             string
}

// FindClusterByToken asks the agent at endpoint for its CA and trusts it only
// if it proves it knows the token.
func FindClusterByToken(ctx context.Context, endpoint string, connectTimeout time.Duration, token jointoken.Token,
	version string,
) (*JoiningCluster, error) {
	address := "https://" + Address(endpoint) + "/v1/join?token=" + url.QueryEscape(token.ID)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, fmt.Errorf("ask %s for its CA: %w", endpoint, err)
	}
	request.Header.Set(VersionHeader, version)
	var offer JoinOffer
	if err := send(uncheckedClient(connectTimeout), request, &offer); err != nil {
		return nil, fmt.Errorf("ask %s for its CA: %w", endpoint, err)
	}
	if !pki.CheckTrustProof(jointoken.ProofKey(token.Hash()), []byte(offer.CACertificate), offer.Proof) {
		return nil, fmt.Errorf("%s cannot prove it knows the join token: a wrong token, or not your cluster", endpoint)
	}
	ca, err := pki.ParseCertificate([]byte(offer.CACertificate))
	if err != nil {
		return nil, fmt.Errorf("read the CA of %s: %w", endpoint, err)
	}
	return &JoiningCluster{
		endpoint: endpoint, version: version, connectTimeout: connectTimeout, token: token,
		CA: ca, CAPEM: offer.CACertificate,
	}, nil
}

// Join asks the cluster to add the host, over TLS checked against the
// trusted CA. It fills in the token, a nonce and the time.
func (c *JoiningCluster) Join(ctx context.Context, request JoinRequest) (JoinAnswer, error) {
	request.Token, request.Time = c.token.String(), time.Now().UTC()
	request.Nonce = make([]byte, 16)
	if _, err := rand.Read(request.Nonce); err != nil {
		return JoinAnswer{}, fmt.Errorf("make a nonce: %w", err)
	}
	body, err := json.Marshal(request)
	if err != nil {
		return JoinAnswer{}, fmt.Errorf("encode the request: %w", err)
	}
	address := "https://" + Address(c.endpoint) + "/v1/join"
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, address, bytes.NewReader(body))
	if err != nil {
		return JoinAnswer{}, fmt.Errorf("join through %s: %w", c.endpoint, err)
	}
	httpRequest.Header.Set(VersionHeader, c.version)
	// longer than the leader takes: if the host gave up on the answer, the
	// state would have the host joined and its token used, but the host
	// itself would have nothing
	client := checkedClient(c.CA, c.connectTimeout)
	client.Timeout = joinClientTimeout
	var answer JoinAnswer
	if err := send(client, httpRequest, &answer); err != nil {
		return JoinAnswer{}, fmt.Errorf("join through %s: %w", c.endpoint, err)
	}
	return answer, nil
}

// uncheckedClient is for a joining host's first contact. There is nothing
// to check the agent against yet, so the trust proof in its answer decides.
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

// checkedClient talks, without a client certificate, to agents of a CA
// that a trust proof made trustworthy.
func checkedClient(ca *x509.Certificate, connectTimeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:   answerTimeout,
		Transport: endpointTransport(pki.ClientTLSToControlPlane(tls.Certificate{}, ca, nil), connectTimeout),
	}
}

// CheckJoinRequest refuses a request that is too old, too new or
// incomplete. It keeps the checks of the request's form in one place. The
// leader checks the rest against the state (state.CheckJoiningHost) and
// Hetzner.
func CheckJoinRequest(request JoinRequest, now time.Time) error {
	if age := now.Sub(request.Time); age > joinMaxAge || age < -joinMaxAge {
		return errors.New("the request is too old (or a clock is off by minutes)")
	}
	switch {
	case len(request.Nonce) < 16:
		return errors.New("the request has no nonce")
	case request.Name == "" || !request.Underlay.IsValid() || !request.PublicAddress.IsValid() || request.ServerID <= 0:
		return errors.New("the request needs the host's name, its underlay address and public address, and its server ID")
	case len(request.PublicKey) == 0 || request.WGPublicKey == "":
		return errors.New("the request needs the host's public keys")
	case len(request.PublicKey) != ed25519.PublicKeySize:
		return errors.New("the host's public key is no Ed25519 key")
	}
	if err := request.Roles.Check(); err != nil {
		return fmt.Errorf("the host's roles: %w", err)
	}
	return nil
}

// newJoinToken makes a join token and commits it. Only its hash goes into
// the state.
func (s *Server) newJoinToken(w http.ResponseWriter, r *http.Request) {
	var request JoinTokenRequest
	if err := decodeStrict(r.Body, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	// the state checks the bounds too; checking here gives a 400
	ttl, err := time.ParseDuration(request.TTL)
	if err != nil || ttl < state.MinJoinTokenTTL || ttl > state.MaxJoinTokenTTL {
		writeError(w, http.StatusBadRequest, fmt.Errorf("a join token lives %s to %s, not %q",
			state.MinJoinTokenTTL, state.MaxJoinTokenTTL, request.TTL))
		return
	}
	if err := request.Roles.Check(); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("the join token's roles: %w", err))
		return
	}
	token, err := jointoken.New()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// no time here: the leader puts its own clock into the command as it
	// commits it, and the handler counts the token's life from there
	cmd, err := state.NewCommand(state.KindAddJoinToken, time.Time{}, actorOf(identityOf(r)),
		state.AddJoinToken{ID: token.ID, Hash: token.Hash(), TTL: ttl, Roles: request.Roles})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	index, ok := s.commit(w, r, cmd, s.Propose)
	if !ok {
		return
	}
	if err := s.config.Cluster.WaitIndex(r.Context(), index); err != nil {
		writeError(w, http.StatusGatewayTimeout, err)
		return
	}
	var expires time.Time
	s.config.Cluster.Read(func(current *state.State) { expires = current.JoinTokens[token.ID].Expires })
	writeJSON(w, http.StatusOK, JoinTokenAnswer{Token: token.String(), Expires: expires})
}

// joinOffer answers a joining host's first contact. It includes a proof
// only while the token is open.
func (s *Server) joinOffer(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("token")
	var caPEM []byte
	var token state.JoinToken
	var err error
	s.config.Cluster.Read(func(current *state.State) {
		if current.Keys != nil {
			caPEM = slices.Clone(current.Keys.CACertificate)
		}
		token, err = current.CheckJoinToken(id, time.Now())
	})
	switch {
	case caPEM == nil:
		writeError(w, http.StatusServiceUnavailable, state.ErrNotBootstrapped)
	case err != nil:
		writeError(w, http.StatusForbidden, err)
	default:
		writeJSON(w, http.StatusOK, JoinOffer{
			CACertificate: string(caPEM), Proof: pki.TrustProof(jointoken.ProofKey(token.Hash), caPEM),
		})
	}
}

// join accepts a join request on any agent. The leader handles it, and a
// follower hands it on.
func (s *Server) join(w http.ResponseWriter, r *http.Request) {
	var request JoinRequest
	if err := decodeStrict(r.Body, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	// what the joining host says is no source; the connection is
	source, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("the request's source %q: %w", r.RemoteAddr, err))
		return
	}
	request.Source = source.Addr().Unmap()
	ctx, cancel := context.WithTimeout(r.Context(), joinTimeout)
	defer cancel()
	var answer JoinAnswer
	leader, here, err := s.waitForLeader(ctx)
	switch {
	case err != nil:
	case here:
		answer, err = s.config.Joins.Join(ctx, request)
	default:
		answer, err = s.forwardJoin(ctx, leader, request)
	}
	s.answerJoin(w, answer, err)
}

// joinTimeout bounds a join on the leader, which asks Hetzner and commits.
// The joining host waits joinClientTimeout, longer than the leader and the
// agent that hands its request on.
const (
	joinTimeout       = time.Minute
	joinClientTimeout = 2 * joinTimeout
)

// internalJoin is where a follower hands a join request to the leader.
func (s *Server) internalJoin(w http.ResponseWriter, r *http.Request) {
	var request JoinRequest
	if err := decodeStrict(r.Body, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	// no second hop: if the lead moved meanwhile, the host asks again
	if !s.config.Cluster.IsLeader() {
		writeError(w, http.StatusServiceUnavailable, errors.New("not the leader; try again"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), joinTimeout)
	defer cancel()
	answer, err := s.config.Joins.Join(ctx, request)
	s.answerJoin(w, answer, err)
}

// answerJoin answers a join. It passes a refusal on as it is, and reports
// any other error as the cluster's fault.
func (s *Server) answerJoin(w http.ResponseWriter, answer JoinAnswer, err error) {
	var statusError *StatusError
	switch {
	case errors.As(err, &statusError):
		writeError(w, statusError.Status, errors.New(statusError.Message))
	case err != nil:
		s.config.Logger.Warn("a join failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, err)
	default:
		writeJSON(w, http.StatusOK, answer)
	}
}

// forwardJoin hands a join request to the leader's internal endpoint.
func (s *Server) forwardJoin(ctx context.Context, id state.HostID, request JoinRequest) (JoinAnswer, error) {
	body, err := json.Marshal(request)
	if err != nil {
		return JoinAnswer{}, fmt.Errorf("encode the request: %w", err)
	}
	address := "https://" + s.config.APIAddress(id) + "/v1/internal/join"
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, address, bytes.NewReader(body))
	if err != nil {
		return JoinAnswer{}, fmt.Errorf("forward to the leader (host %d): %w", id, err)
	}
	httpRequest.Header.Set(VersionHeader, s.config.Version)
	var answer JoinAnswer
	// not the forwarder: its waitTimeout is shorter than the joinTimeout the
	// leader may take. The streamer has no timeout of its own; ctx ends it.
	if err := send(s.streamer, httpRequest, &answer); err != nil {
		var statusError *StatusError
		if errors.As(err, &statusError) {
			return JoinAnswer{}, statusError
		}
		return JoinAnswer{}, fmt.Errorf("forward to the leader (host %d): %w", id, err)
	}
	return answer, nil
}
