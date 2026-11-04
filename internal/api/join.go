package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"time"

	"github.com/webcodingcat/clowder/internal/jointoken"
	"github.com/webcodingcat/clowder/internal/pki"
	"github.com/webcodingcat/clowder/internal/state"
)

// A host joins with a one-time token a user made (clowder host join-token,
// internal/jointoken):
//
//  1. GET /v1/join?token=<id>: the agent sends its CA certificate and, while
//     the token is open, pki.TrustProof over it made with the token's hash
//     (jointoken.ProofKey). The joining host trusts the CA only if the
//     proof matches its token.
//  2. POST /v1/join over TLS checked against that CA: the token itself, the
//     host's name, addresses, server ref and public keys, a nonce and the
//     time. Any agent takes it and hands it to the leader, which checks the
//     token and the server ref, chooses the host's ID, issues its
//     certificate and commits add-host, which uses the token up. The answer
//     carries the certificate, the data key and the other hosts.
//
// Neither request has a client certificate, so each agent takes only so
// many tries of them per minute (limiter).

// JoinOffer is the agent's answer to a joining host's first contact. It
// comes unchecked; the proof makes the CA trustworthy.
type JoinOffer struct {
	CACertificate string `json:"ca_certificate"`
	Proof         []byte `json:"proof"`
}

// JoinRequest asks the leader to add a host.
type JoinRequest struct {
	// Token is the whole join token: it travels only over TLS checked
	// against the CA the proof made trustworthy.
	Token         string     `json:"token"`
	Name          string     `json:"name"`
	Underlay      netip.Addr `json:"underlay"`
	PublicAddress netip.Addr `json:"public_address,omitzero"`
	ServerRef     string     `json:"server_ref"`
	WGPublicKey   string     `json:"wg_public_key"`
	// PublicKey is the host's own key (Ed25519), which its certificate is
	// for; the private half never leaves the host.
	PublicKey []byte `json:"public_key"`
	// Nonce and Time make each request one of its own: the leader takes a
	// request only once, and only for joinMaxAge.
	Nonce []byte    `json:"nonce"`
	Time  time.Time `json:"time"`
	// Source is the address the request came from, which the agent that
	// took it sets: the leader takes a join only from the host's own
	// addresses.
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
	// Peers are the other hosts: the new host builds its WireGuard
	// interfaces to them before Raft can tell it anything.
	Peers []JoinPeer `json:"peers"`
}

// JoinPeer is another host, as the joining host's WireGuard needs it.
type JoinPeer struct {
	ID          state.HostID `json:"id"`
	WGPublicKey string       `json:"wg_public_key"`
	Underlay    netip.Addr   `json:"underlay"`
}

// Joins is the leader's side of a join, which the agent implements: it
// checks the request against the state and the hoster, and commits it.
type Joins interface {
	// Join adds the host. A refusal is a *StatusError the joining host is
	// shown (JoinRefused); any other error is the cluster's.
	Join(ctx context.Context, request JoinRequest) (JoinAnswer, error)
}

// JoinRefused is a refusal of a join, with why.
func JoinRefused(err error) error {
	return &StatusError{Status: http.StatusForbidden, Message: err.Error()}
}

// DefaultJoinTokenTTL is how long a join token opens unless the user says
// otherwise.
const DefaultJoinTokenTTL = state.DefaultJoinTokenTTL

// JoinTokenRequest asks for a join token that lives for TTL, a Go
// duration ("1h", "90m").
type JoinTokenRequest struct {
	TTL string `json:"ttl"`
}

// JoinTokenAnswer is a new join token, the only time it is shown.
type JoinTokenAnswer struct {
	Token   string    `json:"token"`
	Expires time.Time `json:"expires"`
}

// JoiningCluster is a cluster a joining host found and trusts: its CA
// proved it knows the token.
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
	// longer than the leader takes: an answer given up on would leave the
	// host joined in the state, its token used, and nothing on the host
	client := checkedClient(c.CA, c.connectTimeout)
	client.Timeout = joinClientTimeout
	var answer JoinAnswer
	if err := send(client, httpRequest, &answer); err != nil {
		return JoinAnswer{}, fmt.Errorf("join through %s: %w", c.endpoint, err)
	}
	return answer, nil
}

// CheckJoinRequest refuses a request that is too old or too new, or that
// is not whole: the checks of its form, in one place. The leader checks
// the rest against the state (state.CheckJoiningHost) and the hoster.
func CheckJoinRequest(request JoinRequest, now time.Time) error {
	if age := now.Sub(request.Time); age > joinMaxAge || age < -joinMaxAge {
		return errors.New("the request is too old (or a clock is off by minutes)")
	}
	switch {
	case len(request.Nonce) < 16:
		return errors.New("the request has no nonce")
	case request.Name == "" || !request.Underlay.IsValid() || request.ServerRef == "":
		return errors.New("the request needs the host's name, underlay address and server ref")
	case len(request.PublicKey) == 0 || request.WGPublicKey == "":
		return errors.New("the request needs the host's public keys")
	case len(request.PublicKey) != ed25519.PublicKeySize:
		return errors.New("the host's public key is no Ed25519 key")
	}
	return nil
}

// newJoinToken makes a join token and commits it; only its hash goes into
// the state.
func (s *Server) newJoinToken(w http.ResponseWriter, r *http.Request) {
	var request JoinTokenRequest
	if err := decodeStrict(r.Body, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	// the state checks the bounds too; here they come with a 400
	ttl, err := time.ParseDuration(request.TTL)
	if err != nil || ttl < state.MinJoinTokenTTL || ttl > state.MaxJoinTokenTTL {
		writeError(w, http.StatusBadRequest, fmt.Errorf("a join token lives %s to %s, not %q",
			state.MinJoinTokenTTL, state.MaxJoinTokenTTL, request.TTL))
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
		state.AddJoinToken{ID: token.ID, Hash: token.Hash(), TTL: ttl})
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

// joinOffer answers a joining host's first contact: a proof only while
// the token is open.
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
		writeError(w, http.StatusServiceUnavailable, errors.New("the cluster is not bootstrapped yet"))
	case err != nil:
		writeError(w, http.StatusForbidden, err)
	default:
		writeJSON(w, http.StatusOK, JoinOffer{
			CACertificate: string(caPEM), Proof: pki.TrustProof(jointoken.ProofKey(token.Hash), caPEM),
		})
	}
}

// join takes a join request on any agent: the leader handles it, a
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

// joinTimeout bounds a join on the leader: it asks the hoster and commits.
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

// answerJoin answers a join: a refusal as it is, any other error as the
// cluster's.
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
