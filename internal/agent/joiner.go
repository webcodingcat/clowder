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
	"github.com/webcodingcat/clowder/internal/hetzner"
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

	// mu lets one join run at a time: two joins at once would choose the
	// same free ID, and the second would be refused through no fault of
	// its own
	mu sync.Mutex
	// seen holds the nonces of the last joinNonceKept, each with the time
	// it was seen, so the leader takes each request only once. That
	// protects little: whoever saw a request also saw its token, and only
	// the cluster's agents see requests, over TLS. It is the design's
	// insurance against a request sent twice. It lives only in this
	// leader's memory, so a new leader starts with an empty map.
	seen map[string]time.Time
}

// joinNonceKept is how long a nonce is remembered. It is longer than the
// time window in which a request is accepted (api.CheckJoinRequest), on
// either side of the leader's clock.
const joinNonceKept = 15 * time.Minute

// Join checks a request against the state and Hetzner, and adds the
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
	if !current.Configured() {
		return api.JoinAnswer{}, api.JoinRefused(errors.New("the cluster has no ClusterConfiguration yet: apply it " +
			"first, the leader checks the host's server with its Hetzner token"))
	}
	open, err := current.CheckJoinToken(token.ID, now)
	if err != nil {
		return api.JoinAnswer{}, api.JoinRefused(err)
	}
	if subtle.ConstantTimeCompare([]byte(open.Hash), []byte(token.Hash())) != 1 {
		return api.JoinAnswer{}, api.JoinRefused(fmt.Errorf("%w: the secret is wrong", state.ErrJoinTokenRefused))
	}
	if err := open.CheckRoles(request.Roles); err != nil {
		return api.JoinAnswer{}, api.JoinRefused(err)
	}
	id, ok := current.FreeHostID()
	if !ok {
		return api.JoinAnswer{}, api.JoinRefused(fmt.Errorf("the cluster has %d hosts, the most there may be", state.MaxHosts))
	}
	// The certificate is issued before the checks, because the state checks
	// its serial too. A refused join never hands out its certificate.
	certificatePEM, certificate, err := j.issue(current, request, id, now)
	if err != nil {
		return api.JoinAnswer{}, err
	}
	host := state.Host{
		ID: id, Name: request.Name, Roles: request.Roles, ServerID: request.ServerID, Underlay: request.Underlay,
		PublicAddress: request.PublicAddress, WGPublicKey: request.WGPublicKey,
		CertificateSerial: pki.Serial(certificate), CertificateNotAfter: certificate.NotAfter,
	}
	// The state's checks come first: they refuse a server that belongs to
	// another host, without asking Hetzner.
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
	host.ServerChecked = true
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
// addresses. Hetzner confirms that the addresses a host names belong to its
// server, but not that the host is that server. Without this check,
// whoever holds a token could name another server of the project, and
// fencing would later power that server off.
func comesFromHost(source netip.Addr, host state.Host) bool {
	return source == host.Underlay || source == host.PublicAddress
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

// issue makes the joining host's certificate, for its own key, its ID, its
// roles and its addresses.
func (j *joiner) issue(current *state.State, request api.JoinRequest, id state.HostID, now time.Time) (
	[]byte, *x509.Certificate, error,
) {
	if current.Keys == nil {
		return nil, nil, state.ErrNotBootstrapped
	}
	caKeyPEM, err := j.dataKey.Decrypt(current.Keys.CAKey)
	if err != nil {
		return nil, nil, fmt.Errorf("open the CA key: %w", err)
	}
	ca, err := pki.LoadCA(current.Keys.CACertificate, caKeyPEM)
	if err != nil {
		return nil, nil, err
	}
	certificatePEM, err := ca.IssueHost(ed25519.PublicKey(request.PublicKey), int(id), request.Roles,
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

// checkServer asks Hetzner whether the server belongs to the host
// (hetzner.Client.CheckServer). A server that belongs to someone else is a
// refusal; an API that does not answer is not.
func (j *joiner) checkServer(ctx context.Context, current *state.State, host state.Host) error {
	client, err := controller.OpenHetzner(current, j.dataKey.Decrypt)
	if err != nil {
		return fmt.Errorf("check the server of %s: %w", host.Name, err)
	}
	err = client.CheckServer(ctx, host.ServerID, host.Underlay, host.PublicAddress)
	if errors.Is(err, hetzner.ErrOtherServer) {
		return api.JoinRefused(err)
	}
	return err
}

// joinPeers are the hosts to which a joining host builds its WireGuard
// interfaces, the same hosts that readWantedNetwork picks.
func joinPeers(current *state.State, joining state.HostID) []api.JoinPeer {
	var peers []api.JoinPeer
	for _, id := range slices.Sorted(maps.Keys(current.Hosts)) {
		if host := current.Hosts[id]; id != joining && host.Phase != state.Removed {
			peers = append(peers, api.JoinPeer{ID: id, WGPublicKey: host.WGPublicKey, Underlay: host.Underlay})
		}
	}
	return peers
}
