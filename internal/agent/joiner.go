package agent

import (
	"context"
	"crypto/ed25519"
	"crypto/subtle"
	"crypto/x509"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"sync"
	"time"

	"github.com/webcodingcat/clowder/internal/api"
	"github.com/webcodingcat/clowder/internal/controller"
	"github.com/webcodingcat/clowder/internal/datakey"
	"github.com/webcodingcat/clowder/internal/hoster"
	"github.com/webcodingcat/clowder/internal/jointoken"
	"github.com/webcodingcat/clowder/internal/network"
	"github.com/webcodingcat/clowder/internal/pki"
	"github.com/webcodingcat/clowder/internal/state"
)

// joiner is the leader's side of a join (api.Joins); join.go is the side of
// the host that joins.
type joiner struct {
	cluster interface {
		SharedCopy() (*state.State, error)
	}
	propose controller.Propose
	dataKey *datakey.Key
	connect controller.ConnectHoster

	// mu takes one join at a time: two at once would choose the same free
	// ID, and the second be refused for what is no fault of its own
	mu sync.Mutex
	// seen are the nonces of the last joinNonceKept, each with when it was
	// seen: the leader takes a request once. That protects little: whoever
	// saw a request saw its token, and only the cluster's agents see one,
	// over TLS. It is the design's insurance against a request sent twice,
	// and lives in this leader's memory only: a new leader starts afresh.
	seen map[string]time.Time
}

// joinNonceKept is how long a nonce is remembered: longer than a request
// is taken (api.CheckJoinRequest), on either side of the leader's clock.
const joinNonceKept = 15 * time.Minute

// Join checks a request against the state and the hoster, and adds the
// host. A refusal says why, and the token stays open for another try.
// Someone who has only a token's ID learns whether the token is open, and
// nothing more (state.ErrJoinTokenRefused); the other reasons need the
// secret. Joins come one at a time, so a host that starts together with
// others may wait a minute.
func (j *joiner) Join(ctx context.Context, request api.JoinRequest) (api.JoinAnswer, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	now := time.Now()
	if err := api.CheckJoinRequest(request, now); err != nil {
		return api.JoinAnswer{}, api.JoinRefused(err)
	}
	if !j.firstSeen(string(request.Nonce), now) {
		return api.JoinAnswer{}, api.JoinRefused(errors.New("the request was sent before"))
	}
	token, err := jointoken.Parse(request.Token)
	if err != nil {
		return api.JoinAnswer{}, api.JoinRefused(err)
	}
	current, err := j.cluster.SharedCopy()
	if err != nil {
		return api.JoinAnswer{}, fmt.Errorf("read the state for the join: %w", err)
	}
	if current.Config.Revision == 0 {
		return api.JoinAnswer{}, api.JoinRefused(errors.New(
			"the cluster has no cluster.yaml yet: apply it first, the leader checks the host's server with the hoster"))
	}
	open, err := current.CheckJoinToken(token.ID, now)
	if err != nil {
		return api.JoinAnswer{}, api.JoinRefused(err)
	}
	if subtle.ConstantTimeCompare([]byte(open.Hash), []byte(token.Hash())) != 1 {
		return api.JoinAnswer{}, api.JoinRefused(fmt.Errorf("%w: the secret is wrong", state.ErrJoinTokenRefused))
	}
	id, ok := current.FreeHostID()
	if !ok {
		return api.JoinAnswer{}, api.JoinRefused(fmt.Errorf("the cluster has %d hosts, the most there may be", state.MaxHosts))
	}
	// The certificate comes before the checks, as the state checks its
	// serial too. A refused join never hands its certificate out.
	certificatePEM, certificate, err := j.issue(current, request, id, now)
	if err != nil {
		return api.JoinAnswer{}, err
	}
	host := state.Host{
		ID: id, Name: request.Name, ServerRef: request.ServerRef, Underlay: request.Underlay,
		PublicAddress: request.PublicAddress, WGPublicKey: request.WGPublicKey,
		CertificateSerial: pki.Serial(certificate), CertificateNotAfter: certificate.NotAfter,
	}
	// The state's checks come first: they refuse a server ref that another
	// host has, without asking the hoster.
	if err := current.CheckJoiningHost(host); err != nil {
		return api.JoinAnswer{}, api.JoinRefused(err)
	}
	if !comesFromHost(request.Source, host) {
		return api.JoinAnswer{}, api.JoinRefused(fmt.Errorf(
			"the request comes from %s, not from the host's underlay %s or its public address", request.Source, host.Underlay))
	}
	if err := j.checkServer(ctx, current, host); err != nil {
		return api.JoinAnswer{}, err
	}
	host.ServerRefChecked = true
	cmd, err := state.NewCommand(state.KindAddHost, time.Time{}, state.LeaderActor,
		state.AddHost{Token: token.ID, Host: host})
	if err != nil {
		return api.JoinAnswer{}, err
	}
	if _, err := j.propose(ctx, cmd); err != nil {
		var refused *state.Refused
		if errors.As(err, &refused) {
			return api.JoinAnswer{}, api.JoinRefused(refused)
		}
		return api.JoinAnswer{}, fmt.Errorf("commit the join of %s: %w", host.Name, err)
	}
	return api.JoinAnswer{
		Host: id, Certificate: string(certificatePEM), DataKey: j.dataKey.Text(), Peers: joinPeers(current, id),
	}, nil
}

// comesFromHost tells whether a request came from one of the host's own
// addresses. The hoster vouches that the addresses a host names are its
// server's, not that the host is that server; without this check, whoever
// holds a token could name another server of the project, which fencing
// would later power off.
func comesFromHost(source netip.Addr, host state.Host) bool {
	if source == host.Underlay {
		return true
	}
	return host.PublicAddress.IsValid() && source == host.PublicAddress
}

// firstSeen records a nonce and tells whether it is new; the caller holds mu.
func (j *joiner) firstSeen(nonce string, now time.Time) bool {
	if j.seen == nil {
		j.seen = map[string]time.Time{}
	}
	maps.DeleteFunc(j.seen, func(_ string, at time.Time) bool { return now.Sub(at) > joinNonceKept })
	if _, seen := j.seen[nonce]; seen {
		return false
	}
	j.seen[nonce] = now
	return true
}

// issue makes the joining host's certificate, for its own key, its ID and
// its addresses.
func (j *joiner) issue(current *state.State, request api.JoinRequest, id state.HostID, now time.Time) (
	[]byte, *x509.Certificate, error,
) {
	if current.Keys == nil {
		return nil, nil, errors.New("the cluster is not bootstrapped yet")
	}
	caKeyPEM, err := j.dataKey.Open(current.Keys.CAKey)
	if err != nil {
		return nil, nil, fmt.Errorf("open the CA key: %w", err)
	}
	ca, err := pki.LoadCA(current.Keys.CACertificate, caKeyPEM)
	if err != nil {
		return nil, nil, err
	}
	certificatePEM, err := ca.IssueHost(ed25519.PublicKey(request.PublicKey), int(id),
		[]netip.Addr{request.Underlay, network.NodeAddress(id)}, now)
	if err != nil {
		return nil, nil, err
	}
	certificate, err := pki.ParseCertificate(certificatePEM)
	if err != nil {
		return nil, nil, err
	}
	return certificatePEM, certificate, nil
}

// checkServer asks the hoster whether the server ref is the host's
// (hoster.CheckServerRef). A server of another is a refusal; a hoster that
// does not answer is not.
func (j *joiner) checkServer(ctx context.Context, current *state.State, host state.Host) error {
	client, err := controller.OpenHoster(current, j.dataKey.Open, j.connect)
	if err != nil {
		return fmt.Errorf("check the server of %s: %w", host.Name, err)
	}
	err = hoster.CheckServerRef(ctx, client, host.ServerRef, host.Underlay, host.PublicAddress)
	if errors.Is(err, hoster.ErrOtherServer) {
		return api.JoinRefused(err)
	}
	return err
}

// joinPeers are the hosts a joining host builds its WireGuard interfaces
// to, as readWantedNetwork has them.
func joinPeers(current *state.State, joining state.HostID) []api.JoinPeer {
	var peers []api.JoinPeer
	for _, id := range slices.Sorted(maps.Keys(current.Hosts)) {
		if host := current.Hosts[id]; id != joining && host.Phase != state.Removed {
			peers = append(peers, api.JoinPeer{ID: id, WGPublicKey: host.WGPublicKey, Underlay: host.Underlay})
		}
	}
	return peers
}
