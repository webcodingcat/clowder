package daemon

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/webcodingcat/clowder/internal/api"
	"github.com/webcodingcat/clowder/internal/clowderd/daemon/daemontest"
	"github.com/webcodingcat/clowder/internal/hetzner/hetznertest"
	"github.com/webcodingcat/clowder/internal/hostrole"
	"github.com/webcodingcat/clowder/internal/jointoken"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/pki"
	"github.com/webcodingcat/clowder/internal/state"
)

// joinTest is a leader's side of a join over daemontest's cluster, with a real CA
// and a Hetzner project whose server 40 is node0's, 42 has 10.0.0.4 and
// 198.51.100.4, and 43 is a stranger's.
type joinTest struct {
	t       *testing.T
	cluster *daemontest.Cluster
	leader  *joinLeader
	key     ed25519.PrivateKey
}

func newJoinTest(t *testing.T) *joinTest {
	t.Helper()
	cluster := daemontest.NewCluster(t)
	ca, err := pki.NewCA("prod", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	keyPEM, err := ca.KeyPEM()
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cluster.Keys.Encrypt(keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	cluster.State.Keys.CACertificate, cluster.State.Keys.CAKey = ca.CertificatePEM(), encrypted
	setHetznerToken(t, cluster, "the Hetzner token")
	project := hetznertest.New(t, "the Hetzner token")
	project.AddServer(40, netip.MustParseAddr("10.0.0.2"), netip.MustParseAddr("198.51.100.2"))
	project.AddServer(42, netip.MustParseAddr("10.0.0.4"), netip.MustParseAddr("198.51.100.4"))
	project.AddServer(43, netip.MustParseAddr("10.0.0.99"), netip.MustParseAddr("198.51.100.99"))
	cluster.State.Config.Spec.Hetzner.API = project.URL
	key, err := pki.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	return &joinTest{
		t: t, cluster: cluster, key: key,
		leader: &joinLeader{cluster: cluster, propose: cluster.Propose, dataKey: cluster.Keys},
	}
}

// setHetznerToken applies the token to the Secret the configuration names
// for it (clowder-system/hetzner, key token).
func setHetznerToken(t *testing.T, cluster *daemontest.Cluster, token string) {
	t.Helper()
	cluster.PutSecretIn(names.System, "hetzner", map[string]string{"token": token})
}

// token opens a join token for a worker with ingress, as by default.
func (j *joinTest) token() jointoken.Token {
	j.t.Helper()
	return j.tokenFor(hostrole.DefaultJoin)
}

// tokenFor opens a join token for the given roles.
func (j *joinTest) tokenFor(roles hostrole.Set) jointoken.Token {
	j.t.Helper()
	token, err := jointoken.New()
	if err != nil {
		j.t.Fatal(err)
	}
	j.cluster.Apply(state.KindAddJoinToken, state.CLIActor("admin"),
		state.AddJoinToken{ID: token.ID, Hash: token.Hash(), TTL: time.Hour, Roles: roles})
	return token
}

// request is node2's join as a worker with ingress with the token, on
// server 42, from its underlay.
func (j *joinTest) request(token string) api.JoinRequest {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		j.t.Fatal(err)
	}
	return api.JoinRequest{
		Token: token, Name: "node2", Roles: hostrole.DefaultJoin, Underlay: netip.MustParseAddr("10.0.0.4"),
		PublicAddress: netip.MustParseAddr("198.51.100.4"), ServerID: 42,
		WGPublicKey: "wg-2", PublicKey: j.key.Public().(ed25519.PublicKey), Nonce: nonce, Time: time.Now(),
		Source: netip.MustParseAddr("10.0.0.4"),
	}
}

// refused fails the test unless the join is refused with want.
func (j *joinTest) refused(request api.JoinRequest, want string) {
	j.t.Helper()
	_, err := j.leader.Join(context.Background(), request)
	var statusError *api.StatusError
	if !errors.As(err, &statusError) || statusError.Status != http.StatusForbidden || !strings.Contains(err.Error(), want) {
		j.t.Errorf("want a refusal with %q, got %v", want, err)
	}
}

func TestALeaderAddsAHostWithAnOpenTokenAndItsOwnServer(t *testing.T) {
	j := newJoinTest(t)
	token := j.token()
	answer, err := j.leader.Join(context.Background(), j.request(token.String()))
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := pki.ParseCertificate([]byte(answer.Certificate))
	if err != nil {
		t.Fatal(err)
	}
	if answer.Host != 1 || !certificate.PublicKey.(ed25519.PublicKey).Equal(j.key.Public()) ||
		len(answer.Peers) != 1 || answer.Peers[0].ID != 0 {
		t.Errorf("answer %+v", answer)
	}
	if answer.DataKey != "" {
		t.Error("a worker got the data key")
	}
	// what the joining host checks (join.checkJoinCertificate): signed by
	// the CA, for this host, with the roles it asked for
	ca, err := pki.ParseCertificate(j.cluster.State.Keys.CACertificate)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := pki.Verify(ca, certificate, func(string) bool { return false }, time.Now())
	if err != nil || identity.Role != pki.RoleHost || identity.HostID != int(answer.Host) ||
		!slices.Equal(identity.HostRoles, hostrole.DefaultJoin) {
		t.Errorf("the certificate names %+v (%v)", identity, err)
	}
	j.cluster.Read(func(current *state.State) {
		host := current.Hosts[1]
		if host == nil || host.Phase != state.Joining || !host.ServerChecked || host.CertificateSerial != pki.Serial(certificate) ||
			!slices.Equal(host.Roles, hostrole.DefaultJoin) {
			t.Errorf("host 1: %+v", host)
		}
		if _, open := current.JoinTokens[token.ID]; open {
			t.Error("the token is still open")
		}
	})
	j.refused(j.request(token.String()), "it is unknown, or a host joined with it")
}

func TestOnlyAControlPlaneHostGetsTheDataKey(t *testing.T) {
	j := newJoinTest(t)
	request := j.request(j.tokenFor(hostrole.DefaultInit).String())
	request.Roles = hostrole.DefaultInit
	answer, err := j.leader.Join(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if answer.DataKey != j.cluster.Keys.Text() {
		t.Errorf("a control-plane host got the data key %q", answer.DataKey)
	}
}

func TestAJoinWithRolesItsTokenDoesNotAllowIsRefused(t *testing.T) {
	j := newJoinTest(t)
	token := j.token()
	controlPlane := j.request(token.String())
	controlPlane.Roles = hostrole.DefaultInit
	j.refused(controlPlane, "the join token allows the roles worker,ingress, not control-plane,worker,ingress")
	j.cluster.Read(func(current *state.State) {
		if _, open := current.JoinTokens[token.ID]; !open || len(current.Hosts) != 1 {
			t.Errorf("a refused join changed the state: tokens %v, hosts %d", current.JoinTokens, len(current.Hosts))
		}
	})
	worker := j.request(token.String())
	worker.Roles = hostrole.Set{hostrole.Worker}
	if _, err := j.leader.Join(context.Background(), worker); err != nil {
		t.Errorf("a join with fewer roles than the token allows: %v", err)
	}
}

func TestAJoinWithAWrongTokenIsRefused(t *testing.T) {
	j := newJoinTest(t)
	token := j.token()
	other, err := jointoken.New()
	if err != nil {
		t.Fatal(err)
	}
	_, otherSecret, _ := strings.Cut(other.String(), ".")
	j.refused(j.request(other.String()), "it is unknown")
	j.refused(j.request(token.ID+"."+otherSecret), "the secret is wrong")
	j.refused(j.request("no token"), "the join token has no dot")

	j.cluster.Lock()
	expired := j.cluster.State.JoinTokens[token.ID]
	expired.Expires = time.Now().Add(-time.Second)
	j.cluster.State.JoinTokens[token.ID] = expired
	j.cluster.Unlock()
	j.refused(j.request(token.String()), "it expired")
}

func TestAJoinIsTakenOnceAndOnlyWhenRecent(t *testing.T) {
	j := newJoinTest(t)
	token := j.token()
	request := j.request(token.String())
	request.ServerID = 43 // refused, so the token stays open for the replay
	j.refused(request, "another server")
	j.refused(request, "the request was sent before")
	old := j.request(token.String())
	old.Time = time.Now().Add(-10 * time.Minute)
	j.refused(old, "too old")
}

func TestAJoinWithTheServerOfAnotherIsRefused(t *testing.T) {
	j := newJoinTest(t)
	token := j.token()
	stranger := j.request(token.String())
	stranger.ServerID = 43
	j.refused(stranger, "server 43 has the private addresses [10.0.0.99], not the host's underlay 10.0.0.4: it is another server")
	// the addresses of another server of the project, from somewhere else:
	// Hetzner would vouch for them
	elsewhere := j.request(token.String())
	elsewhere.Underlay, elsewhere.Source = netip.MustParseAddr("10.0.0.99"), netip.MustParseAddr("192.0.2.7")
	elsewhere.PublicAddress, elsewhere.ServerID = netip.MustParseAddr("198.51.100.99"), 43
	j.refused(elsewhere, "the request comes from 192.0.2.7, not from the host's underlay 10.0.0.99")
	public := j.request(token.String())
	public.PublicAddress, public.Source = netip.MustParseAddr("198.51.100.5"), netip.MustParseAddr("198.51.100.5")
	j.refused(public, "server 42 has the public address 198.51.100.4, not the host's 198.51.100.5")
	taken := j.request(token.String())
	taken.ServerID = 40
	j.refused(taken, "host node0 has the server 40 already")
	unknown := j.request(token.String())
	unknown.ServerID = 44
	j.refused(unknown, "the project has no such server: it is another server")
	setHetznerToken(t, j.cluster, "a token Hetzner refuses")
	if _, err := j.leader.Join(context.Background(), j.request(token.String())); err == nil ||
		errors.As(err, new(*api.StatusError)) {
		t.Errorf("an API that cannot answer is no refusal: %v", err)
	}
	j.cluster.Read(func(current *state.State) {
		if _, open := current.JoinTokens[token.ID]; !open || len(current.Hosts) != 1 {
			t.Errorf("a refused join changed the state: tokens %v, hosts %d", current.JoinTokens, len(current.Hosts))
		}
	})
}
