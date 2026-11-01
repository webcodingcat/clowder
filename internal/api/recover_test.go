package api

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/webcodingcat/clowder/internal/pki"
	"github.com/webcodingcat/clowder/internal/recoverycode"
	"github.com/webcodingcat/clowder/internal/state"
)

// withRealCA puts the test's CA with its sealed key into every host's
// state, so the agents can issue certificates.
func (c *testCluster) withRealCA() {
	c.t.Helper()
	keyPEM, err := c.ca.KeyPEM()
	if err != nil {
		c.t.Fatal(err)
	}
	sealed, err := c.apis[0].config.Keys.Seal(keyPEM)
	if err != nil {
		c.t.Fatal(err)
	}
	for _, cluster := range c.clusters {
		cluster.state.Keys.CACertificate = c.ca.CertificatePEM()
		cluster.state.Keys.CAKey = sealed
	}
}

// code is what clowderd user recovery-code gives for a user now.
func (c *testCluster) code(user string) recoverycode.Code {
	c.t.Helper()
	key, err := c.apis[0].config.Keys.Derive(recoverycode.Purpose)
	if err != nil {
		c.t.Fatal(err)
	}
	return recoverycode.Make(key, user, time.Now())
}

func (c *testCluster) seed(host int) string {
	return c.servers[host].Listener.Addr().String()
}

func TestARecoveredUserIsBackAndTheOldCertificateIsOut(t *testing.T) {
	cluster := newTestCluster(t)
	cluster.withRealCA()
	stolen := cluster.userCertificate("alice")
	cmd, err := state.NewCommand(state.KindAddUser, time.Now(), state.LeaderActor,
		state.AddUser{Name: "alice", Serial: pki.Serial(stolen.Leaf), NotAfter: stolen.Leaf.NotAfter})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cluster.clusters[0].Propose(context.Background(), cmd); err != nil {
		t.Fatal(err)
	}

	// through the follower, which forwards recover-user to the leader
	found, err := FindClusterByCode(context.Background(), []string{"127.0.0.1:1", cluster.seed(1)}, "alice",
		cluster.code("alice"), testVersion)
	if err != nil {
		t.Fatal(err)
	}
	key, err := pki.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	certificatePEM, err := found.Recover(context.Background(), key.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	keyPEM, err := pki.EncodeKey(key)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := tls.X509KeyPair([]byte(certificatePEM), keyPEM)
	if err != nil {
		t.Fatal(err)
	}

	var status Status
	if err := cluster.client(recovered, testVersion, 0).Get(context.Background(), "/v1/status", &status); err != nil {
		t.Errorf("the new certificate: %v", err)
	}
	err = cluster.client(stolen, testVersion, 0).Get(context.Background(), "/v1/status", &status)
	if statusOf(err) != http.StatusUnauthorized {
		t.Errorf("the stolen certificate: want 401, got %v", err)
	}
	if last := cluster.clusters[0].state.Audit[len(cluster.clusters[0].state.Audit)-1]; last.Kind != state.KindRecoverUser ||
		last.Actor != "user/alice" {
		t.Errorf("audit %+v", last)
	}
}

func TestTheRecoveryTellsNobodyWhichUsersThereAre(t *testing.T) {
	cluster := newTestCluster(t)
	cluster.withRealCA()
	// the offer for a user that does not exist proves the code as for one that does
	if _, err := FindClusterByCode(context.Background(), []string{cluster.seed(0)}, "mallory",
		cluster.code("mallory"), testVersion); err != nil {
		t.Fatalf("offer for an unknown user: %v", err)
	}
	if _, err := FindClusterByCode(context.Background(), []string{cluster.seed(0)}, "alice",
		cluster.code("bob"), testVersion); err == nil || !strings.Contains(err.Error(), "cannot prove") {
		t.Errorf("another user's code: %v", err)
	}

	key, err := pki.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	tryRecover := func(user string, code recoverycode.Code) error {
		found := &RecoveringCluster{
			seed: cluster.seed(0), version: testVersion, user: user, code: code, CA: cluster.ca.Certificate,
		}
		_, err := found.Recover(context.Background(), key.Public().(ed25519.PublicKey))
		return err
	}
	unknown := tryRecover("mallory", cluster.code("mallory"))
	wrong := tryRecover("alice", cluster.code("bob"))
	if statusOf(unknown) != http.StatusUnauthorized || unknown.Error() != wrong.Error() {
		t.Errorf("an unknown user (%v) and a wrong code (%v) must get the same answer", unknown, wrong)
	}
}

func TestOnlyTheCallersOwnCodeRotatesTheRecoveryKey(t *testing.T) {
	cluster := newTestCluster(t)
	client := cluster.client(cluster.userCertificate("alice"), testVersion, 0)
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	recipient := identity.Recipient().String()

	_, err = client.Post(context.Background(), "/v1/recovery/rotate", RotateRequest{Recipient: recipient})
	if statusOf(err) != http.StatusBadRequest {
		t.Errorf("without a code: want 400, got %v", err)
	}
	_, err = client.Post(context.Background(), "/v1/recovery/rotate",
		RotateRequest{Recipient: recipient, Code: cluster.code("bob").String()})
	if statusOf(err) != http.StatusUnauthorized {
		t.Errorf("with bob's code: want 401, got %v", err)
	}
	if _, err := client.Post(context.Background(), "/v1/recovery/rotate",
		RotateRequest{Recipient: recipient, Code: cluster.code("alice").String()}); err != nil {
		t.Fatal(err)
	}
	var encryption Encryption
	if err := client.Get(context.Background(), "/v1/encryption", &encryption); err != nil {
		t.Fatal(err)
	}
	if encryption.Recovery != recipient {
		t.Errorf("recovery %s, want %s", encryption.Recovery, recipient)
	}
}

func TestTheRecoveryTakesOnlySoManyTries(t *testing.T) {
	cluster := newTestCluster(t)
	cluster.withRealCA()
	for try := range triesPerSender + 1 {
		_, err := FindClusterByCode(context.Background(), []string{cluster.seed(0)}, "alice",
			cluster.code("alice"), testVersion)
		switch {
		case try < triesPerSender && err != nil:
			t.Fatalf("try %d: %v", try+1, err)
		case try == triesPerSender && statusOf(err) != http.StatusTooManyRequests:
			t.Errorf("one try too many: %v", err)
		}
	}
}

func TestTheLimiterCountsPerSenderAndInAll(t *testing.T) {
	var limits limiter
	now := time.Unix(1_800_000_000, 0)
	one := netip.MustParseAddr("203.0.113.1")
	for range triesPerSender {
		if !limits.allow(one, now) {
			t.Fatal("a try under the limit was refused")
		}
	}
	if limits.allow(one, now) {
		t.Error("the sender's try over the limit was taken")
	}
	for index := range triesInAll - triesPerSender {
		limits.allow(netip.AddrFrom4([4]byte{198, 51, byte(index / 5), byte(index % 5)}), now)
	}
	if limits.allow(netip.MustParseAddr("192.0.2.1"), now) {
		t.Error("a new sender was taken over the limit in all")
	}
	if !limits.allow(one, now.Add(time.Minute)) {
		t.Error("the next minute starts afresh")
	}
}

func TestSendersCountByAddressAndIPv6By64(t *testing.T) {
	tests := map[string]string{
		"203.0.113.7:4321":                "203.0.113.7",
		"[::ffff:203.0.113.7]:4321":       "203.0.113.7",
		"[2001:db8:1:2:3:4:5:6]:4321":     "2001:db8:1:2::",
		"[2001:db8:1:2:ffff:ffff:1:1]:80": "2001:db8:1:2::",
	}
	for remote, want := range tests {
		if got := senderOf(&http.Request{RemoteAddr: remote}); got.String() != want {
			t.Errorf("%s counts as %s, want %s", remote, got, want)
		}
	}
}
