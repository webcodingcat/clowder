package join

import (
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/webcodingcat/clowder/internal/api"
	"github.com/webcodingcat/clowder/internal/clowderd/hostfiles"
	"github.com/webcodingcat/clowder/internal/hostrole"
	"github.com/webcodingcat/clowder/internal/network"
	"github.com/webcodingcat/clowder/internal/pki"
)

// fakeNetwork sets up no interface; with fails set, it fails to.
type fakeNetwork struct{ fails bool }

func (f fakeNetwork) SetUpInterface(context.Context, netip.Addr) error {
	if f.fails {
		return syscall.ENODEV
	}
	return nil
}

// answered is the cluster's answer to node2's join as a worker with
// ingress, for the key certified: host 1, with node0 as its peer. It
// returns the cluster's CA too.
func answered(t *testing.T, certified ed25519.PrivateKey) (caPEM string, answer api.JoinAnswer) {
	t.Helper()
	ca, err := pki.NewCA("prod", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	certificatePEM, err := ca.IssueHost(certified.Public().(ed25519.PublicKey), 1, hostrole.DefaultJoin,
		[]netip.Addr{netip.MustParseAddr("10.0.0.4"), network.NodeAddress(1)}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return string(ca.CertificatePEM()), api.JoinAnswer{
		Host: 1, Certificate: string(certificatePEM),
		Peers: []api.JoinPeer{{ID: 0, WGPublicKey: "wg", Underlay: netip.MustParseAddr("10.0.0.2")}},
	}
}

// joinedFiles are the files of a host that made the key kept and got the
// cluster's answer for the key certified, as join.json keeps them before
// anything else is written.
func joinedFiles(t *testing.T, certified, kept ed25519.PrivateKey) hostfiles.Files {
	t.Helper()
	caPEM, answer := answered(t, certified)
	wireGuardKey, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM, err := pki.EncodeKey(kept)
	if err != nil {
		t.Fatal(err)
	}
	files := hostfiles.Files{Dir: t.TempDir()}
	saved := joining{
		HostKey: keyPEM, WireGuardKey: wireGuardKey.Bytes(), CA: caPEM, Answer: &answer,
		Host: &hostfiles.Host{
			ID: answer.Host, Name: "node2", Roles: hostrole.DefaultJoin, NodeAddress: network.NodeAddress(answer.Host),
			Underlay: netip.MustParseAddr("10.0.0.4"), PublicAddress: netip.MustParseAddr("198.51.100.4"),
			ServerID: 42, RaftPort: hostfiles.RaftPort, APIPort: hostfiles.APIPort,
		},
	}
	if err := writeJoining(files, saved); err != nil {
		t.Fatal(err)
	}
	return files
}

// node2Options are what cloud-init gives node2's clowderd join.
func node2Options() Options {
	return Options{
		Name: "node2", Roles: hostrole.DefaultJoin, Underlay: netip.MustParseAddr("10.0.0.4"),
		PublicAddress: netip.MustParseAddr("198.51.100.4"), ServerID: 42,
	}
}

func newKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	key, err := pki.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestAJoinGoesOnFromItsAnswerAndKeepsTheMeshForTheFirstStart(t *testing.T) {
	key := newKey(t)
	files := joinedFiles(t, key, key)
	// no agent to join through: with the answer in join.json, Join asks the cluster nothing
	options := node2Options()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := Join(context.Background(), files, options, fakeNetwork{fails: true}, logger); err == nil {
		t.Fatal("a join whose interface did not come up")
	}
	if err := Join(context.Background(), files, options, fakeNetwork{}, logger); err != nil {
		t.Fatalf("the second run: %v", err)
	}
	host, err := files.ReadHost()
	if err != nil || host.ID != 1 || host.NodeAddress != network.NodeAddress(1) || host.RaftPort != hostfiles.RaftPort {
		t.Errorf("host.json %+v, %v", host, err)
	}
	if _, err := files.ReadCredentials(host.Roles); err != nil {
		t.Errorf("the credentials: %v", err)
	}
	if _, err := os.Stat(files.DataKey()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a worker keeps a data key: %v", err)
	}
	if _, err := os.Stat(files.JoinFile()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("join.json is still there: %v", err)
	}
	mesh, err := files.ReadMesh()
	if err != nil {
		t.Fatal(err)
	}
	want := network.Peer{ID: 0, PublicKey: "wg", Underlay: netip.MustParseAddr("10.0.0.2")}
	if mesh.Self != 1 || len(mesh.Peers) != 1 || mesh.Peers[0] != want {
		t.Errorf("net.json %+v, want host 1 with peer %+v", mesh, want)
	}
}

func TestAJoinTakesNoCertificateForAnotherKey(t *testing.T) {
	files := joinedFiles(t, newKey(t), newKey(t))
	err := Join(context.Background(), files, node2Options(), fakeNetwork{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil || !strings.Contains(err.Error(), "not for this host's key") {
		t.Errorf("a certificate for another key: %v", err)
	}
}

func TestAJoinTakesNoCertificateForOtherRoles(t *testing.T) {
	key := newKey(t)
	caPEM, answer := answered(t, key)
	if err := checkJoinCertificate(caPEM, answer, key, hostrole.DefaultJoin); err != nil {
		t.Errorf("the certificate for the roles asked for: %v", err)
	}
	err := checkJoinCertificate(caPEM, answer, key, hostrole.Set{hostrole.Worker})
	if err == nil || !strings.Contains(err.Error(), "names the roles worker,ingress, not worker") {
		t.Errorf("a certificate for other roles: %v", err)
	}
}
