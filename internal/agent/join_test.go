package agent

import (
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/webcodingcat/clowder/internal/agent/agenttest"
	"github.com/webcodingcat/clowder/internal/api"
	"github.com/webcodingcat/clowder/internal/jointoken"
	"github.com/webcodingcat/clowder/internal/network"
	"github.com/webcodingcat/clowder/internal/pki"
	"github.com/webcodingcat/clowder/internal/state"
)

// joinTest is a leader's joiner over agenttest's cluster, with a real CA
// and a hoster whose server 42 is 10.0.0.4 and whose server 43 is a
// stranger's.
type joinTest struct {
	t       *testing.T
	cluster *agenttest.Cluster
	joiner  *joiner
	hoster  *fakeHoster
	key     ed25519.PrivateKey
}

func newJoinTest(t *testing.T) *joinTest {
	t.Helper()
	cluster := agenttest.NewCluster(t)
	ca, err := pki.NewCA("prod", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	keyPEM, err := ca.KeyPEM()
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := cluster.Keys.Seal(keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	cluster.State.Keys.CACertificate, cluster.State.Keys.CAKey = ca.CertificatePEM(), sealed
	sealedToken, err := cluster.Keys.Seal([]byte("the hoster's token"))
	if err != nil {
		t.Fatal(err)
	}
	cluster.State.ClusterSecrets["hoster.token"] = state.SealedSecret{Value: sealedToken, Digest: "d"}
	cluster.State.Hosts[0].ServerRef = "40"
	hoster := &fakeHoster{servers: map[string][]netip.Addr{
		"40": {netip.MustParseAddr("10.0.0.2")},
		"42": {netip.MustParseAddr("10.0.0.4")},
		"43": {netip.MustParseAddr("10.0.0.99")},
	}}
	key, err := pki.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	return &joinTest{
		t: t, cluster: cluster, hoster: hoster, key: key,
		joiner: &joiner{cluster: cluster, propose: cluster.Propose, dataKey: cluster.Keys, connect: hoster.connect},
	}
}

// token opens a join token.
func (j *joinTest) token() jointoken.Token {
	j.t.Helper()
	token, err := jointoken.New()
	if err != nil {
		j.t.Fatal(err)
	}
	j.cluster.Apply(state.KindAddJoinToken, state.UserActor("alice"),
		state.AddJoinToken{ID: token.ID, Hash: token.Hash(), TTL: time.Hour})
	return token
}

// request is node2's join with the token, on server 42, from its underlay.
func (j *joinTest) request(token string) api.JoinRequest {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		j.t.Fatal(err)
	}
	return api.JoinRequest{
		Token: token, Name: "node2", Underlay: netip.MustParseAddr("10.0.0.4"), ServerRef: "42",
		WGPublicKey: "wg-2", PublicKey: j.key.Public().(ed25519.PublicKey), Nonce: nonce, Time: time.Now(),
		Source: netip.MustParseAddr("10.0.0.4"),
	}
}

// refused fails the test unless the join is refused with want.
func (j *joinTest) refused(request api.JoinRequest, want string) {
	j.t.Helper()
	_, err := j.joiner.Join(context.Background(), request)
	var statusError *api.StatusError
	if !errors.As(err, &statusError) || statusError.Status != http.StatusForbidden || !strings.Contains(err.Error(), want) {
		j.t.Errorf("want a refusal with %q, got %v", want, err)
	}
}

func TestALeaderAddsAHostWithAnOpenTokenAndItsOwnServer(t *testing.T) {
	j := newJoinTest(t)
	token := j.token()
	answer, err := j.joiner.Join(context.Background(), j.request(token.String()))
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := pki.ParseCertificate([]byte(answer.Certificate))
	if err != nil {
		t.Fatal(err)
	}
	if answer.Host != 1 || !certificate.PublicKey.(ed25519.PublicKey).Equal(j.key.Public()) ||
		answer.DataKey != j.cluster.Keys.Text() || len(answer.Peers) != 1 || answer.Peers[0].ID != 0 {
		t.Errorf("answer %+v", answer)
	}
	j.cluster.Read(func(current *state.State) {
		host := current.Hosts[1]
		if host == nil || host.Phase != state.Joining || !host.ServerRefChecked || host.CertificateSerial != pki.Serial(certificate) {
			t.Errorf("host 1: %+v", host)
		}
		if _, open := current.JoinTokens[token.ID]; open {
			t.Error("the token is still open")
		}
	})
	j.refused(j.request(token.String()), "it is unknown, or a host joined with it")
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
	request.ServerRef = "43" // refused, so the token stays open for the replay
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
	stranger.ServerRef = "43"
	j.refused(stranger, "server 43 has the addresses [10.0.0.99], not the host's 10.0.0.4: it is another server")
	// the addresses of another server of the project, from somewhere else:
	// the hoster would vouch for them
	elsewhere := j.request(token.String())
	elsewhere.Underlay, elsewhere.Source = netip.MustParseAddr("10.0.0.99"), netip.MustParseAddr("192.0.2.7")
	elsewhere.ServerRef = "43"
	j.refused(elsewhere, "the request comes from 192.0.2.7, not from the host's underlay 10.0.0.99")
	public := j.request(token.String())
	public.PublicAddress, public.Source = netip.MustParseAddr("198.51.100.4"), netip.MustParseAddr("198.51.100.4")
	j.refused(public, "not the host's 198.51.100.4")
	taken := j.request(token.String())
	taken.ServerRef = "40"
	j.refused(taken, "host node0 has the server ref 40 already")
	unknown := j.request(token.String())
	unknown.ServerRef = "44"
	if _, err := j.joiner.Join(context.Background(), unknown); err == nil || errors.As(err, new(*api.StatusError)) {
		t.Errorf("a hoster that cannot answer is no refusal: %v", err)
	}
	j.cluster.Read(func(current *state.State) {
		if _, open := current.JoinTokens[token.ID]; !open || len(current.Hosts) != 1 {
			t.Errorf("a refused join changed the state: tokens %v, hosts %d", current.JoinTokens, len(current.Hosts))
		}
	})
}

// joinedFiles are the files of a host that made its keys and got the
// cluster's answer, as join.json keeps them before anything else is
// written.
func (j *joinTest) joinedFiles(key ed25519.PrivateKey) Files {
	j.t.Helper()
	answer, err := j.joiner.Join(context.Background(), j.request(j.token().String()))
	if err != nil {
		j.t.Fatal(err)
	}
	wireGuardKey, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		j.t.Fatal(err)
	}
	keyPEM, err := pki.EncodeKey(key)
	if err != nil {
		j.t.Fatal(err)
	}
	files := Files{Dir: j.t.TempDir()}
	saved := joining{
		HostKey: keyPEM, WireGuardKey: wireGuardKey.Bytes(),
		CA: string(j.cluster.State.Keys.CACertificate), Answer: &answer,
		Host: &Host{
			ID: answer.Host, Name: "node2", NodeAddress: network.NodeAddress(answer.Host),
			Underlay: netip.MustParseAddr("10.0.0.4"), ServerRef: "42", RaftPort: RaftPort, APIPort: APIPort,
		},
	}
	if err := files.writeJoining(saved); err != nil {
		j.t.Fatal(err)
	}
	return files
}

func TestAJoinGoesOnFromItsAnswerAndKeepsTheMeshForTheFirstStart(t *testing.T) {
	j := newJoinTest(t)
	files := j.joinedFiles(j.key)
	// no agent to join through: with the answer in join.json, Join asks the cluster nothing
	options := JoinOptions{Name: "node2", Underlay: netip.MustParseAddr("10.0.0.4"), ServerRef: "42"}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := Join(context.Background(), files, options, &fakeNetwork{interfaceFails: true}, logger); err == nil {
		t.Fatal("a join whose interface did not come up")
	}
	if err := Join(context.Background(), files, options, &fakeNetwork{}, logger); err != nil {
		t.Fatalf("the second run: %v", err)
	}
	host, err := files.readHost()
	if err != nil || host.ID != 1 || host.NodeAddress != network.NodeAddress(1) || host.RaftPort != RaftPort {
		t.Errorf("host.json %+v, %v", host, err)
	}
	if _, err := files.readCredentials(); err != nil {
		t.Errorf("the credentials: %v", err)
	}
	if _, err := os.Stat(files.joinFile()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("join.json is still there: %v", err)
	}
	restarted := &fakeNetwork{}
	if err := restoreMesh(context.Background(), files, 1, restarted); err != nil {
		t.Fatal(err)
	}
	want := network.Peer{ID: 0, PublicKey: "wg", Underlay: netip.MustParseAddr("10.0.0.2")}
	if len(restarted.meshes) != 1 || restarted.meshes[0].Self != 1 || len(restarted.meshes[0].Peers) != 1 ||
		restarted.meshes[0].Peers[0] != want {
		t.Errorf("restored %+v, want host 1 with peer %+v", restarted.meshes, want)
	}
	if err := restoreMesh(context.Background(), files, 2, &fakeNetwork{}); err == nil {
		t.Error("host 2 took the mesh of host 1")
	}
}

func TestAJoinTakesNoCertificateForAnotherKey(t *testing.T) {
	j := newJoinTest(t)
	other, err := pki.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	files := j.joinedFiles(other)
	options := JoinOptions{Name: "node2", Underlay: netip.MustParseAddr("10.0.0.4"), ServerRef: "42"}
	err = Join(context.Background(), files, options, &fakeNetwork{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil || !strings.Contains(err.Error(), "not for this host's key") {
		t.Errorf("a certificate for another key: %v", err)
	}
}

func TestAHostWithoutNetJSONStartsWithoutAMesh(t *testing.T) {
	files := Files{Dir: t.TempDir()}
	hostNetwork := &fakeNetwork{}
	if err := restoreMesh(context.Background(), files, 0, hostNetwork); err != nil || len(hostNetwork.meshes) != 0 {
		t.Errorf("meshes %v, %v", hostNetwork.meshes, err)
	}
	if err := os.WriteFile(files.meshFile(), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := restoreMesh(context.Background(), files, 0, hostNetwork); err == nil {
		t.Error("a broken net.json was read")
	}
}
