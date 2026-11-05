package api

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"net/http"
	"net/netip"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/webcodingcat/clowder/internal/pki"
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

func (c *testCluster) endpoint(id int) string {
	return c.servers[id].Listener.Addr().String()
}

// newCredentials asks agent id, as host id, for a CLI certificate, as
// clowderd credentials does.
func (c *testCluster) newCredentials(id int, revokeOlder bool) (tls.Certificate, error) {
	c.t.Helper()
	key, err := pki.NewKey()
	if err != nil {
		c.t.Fatal(err)
	}
	host := c.client(c.hostCertificate(state.HostID(id)), testVersion, id)
	request := CredentialsRequest{PublicKey: key.Public().(ed25519.PublicKey), RevokeOlder: revokeOlder}
	var answer CredentialsAnswer
	if err := host.Act(context.Background(), "/v1/internal/credentials", request, &answer); err != nil {
		return tls.Certificate{}, err
	}
	if answer.CACertificate != string(c.ca.CertificatePEM()) {
		c.t.Errorf("the answer names another CA")
	}
	keyPEM, err := pki.EncodeKey(key)
	if err != nil {
		c.t.Fatal(err)
	}
	certificate, err := tls.X509KeyPair([]byte(answer.Certificate), keyPEM)
	if err != nil {
		c.t.Fatal(err)
	}
	return certificate, nil
}

func TestAHostHandsOutCredentialsAndTheOldOnesStayUnlessRevoked(t *testing.T) {
	cluster := newTestCluster(t)
	cluster.withRealCA()
	old := cluster.cliCertificate()
	issued, err := cluster.newCredentials(1, false)
	if err != nil {
		t.Fatal(err)
	}
	for name, certificate := range map[string]tls.Certificate{"new": issued, "old": old} {
		if err := cluster.client(certificate, testVersion, 0).Get(context.Background(), "/v1/status", &Status{}); err != nil {
			t.Errorf("the %s certificate: %v", name, err)
		}
	}
}

func TestRevokeOlderLocksOutEveryCLICertificateBeforeTheNewOne(t *testing.T) {
	cluster := newTestCluster(t)
	cluster.withRealCA()
	// issued an hour before: certificates start a clock skew back, and one of
	// the same second would stay valid
	stolen := cluster.certificate(func(public ed25519.PublicKey) ([]byte, error) {
		return cluster.ca.IssueCLI(public, time.Now().Add(-time.Hour))
	})
	// the follower's fake state does not follow the leader's: it applies the
	// cutoff a moment after the leader, and only then answers
	applied := make(chan struct{})
	go func() {
		defer close(applied)
		for {
			var cutoff time.Time
			var index uint64
			cluster.clusters[0].Read(func(current *state.State) { cutoff, index = current.CLICutoff, current.Index })
			if !cutoff.IsZero() {
				time.Sleep(100 * time.Millisecond)
				cluster.clusters[1].Read(func(current *state.State) { current.CLICutoff, current.Index = cutoff, index })
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	// through the follower, which forwards set-cli-cutoff to the leader
	issued, err := cluster.newCredentials(1, true)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-applied:
	default:
		t.Error("the follower answered before it applied the cutoff")
	}
	var cutoff time.Time
	cluster.clusters[0].Read(func(current *state.State) { cutoff = current.CLICutoff })
	if !cutoff.Equal(issued.Leaf.NotBefore) {
		t.Fatalf("cutoff %v, want the new certificate's start %v", cutoff, issued.Leaf.NotBefore)
	}

	for id := range 2 {
		if err := cluster.client(issued, testVersion, id).Get(context.Background(), "/v1/status", &Status{}); err != nil {
			t.Errorf("agent %d, the new certificate: %v", id, err)
		}
		err := cluster.client(stolen, testVersion, id).Get(context.Background(), "/v1/status", &Status{})
		if statusOf(err) != http.StatusUnauthorized {
			t.Errorf("agent %d, the stolen certificate: want 401, got %v", id, err)
		}
	}
	var last state.AuditEntry
	cluster.clusters[0].Read(func(current *state.State) { last = current.Audit[len(current.Audit)-1] })
	if last.Kind != state.KindSetCLICutoff || last.Actor != state.HostActor(1) {
		t.Errorf("audit %+v", last)
	}
}

func TestOnlyAHostAsksForCredentials(t *testing.T) {
	cluster := newTestCluster(t)
	cluster.withRealCA()
	cli := cluster.client(cluster.cliCertificate(), testVersion, 0)
	key, err := pki.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	err = cli.Act(context.Background(), "/v1/internal/credentials",
		CredentialsRequest{PublicKey: key.Public().(ed25519.PublicKey)}, &CredentialsAnswer{})
	if statusOf(err) != http.StatusForbidden {
		t.Errorf("the CLI asked for credentials: want 403, got %v", err)
	}
}

func TestTheCLIRotatesTheRecoveryKey(t *testing.T) {
	cluster := newTestCluster(t)
	client := cluster.client(cluster.cliCertificate(), testVersion, 0)
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	recipient := identity.Recipient().String()
	_, err = client.Post(context.Background(), "/v1/recovery/rotate", RotateRequest{Recipient: "age1-no-key"})
	if statusOf(err) != http.StatusBadRequest {
		t.Errorf("no age key: want 400, got %v", err)
	}
	if _, err := client.Post(context.Background(), "/v1/recovery/rotate", RotateRequest{Recipient: recipient}); err != nil {
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

func TestTheBootstrapTakesOnlySoManyTries(t *testing.T) {
	cluster := newTestCluster(t)
	cluster.withRealCA()
	for try := range triesPerSender + 1 {
		request, err := http.NewRequest(http.MethodGet, "https://"+cluster.endpoint(0)+"/v1/bootstrap", nil)
		if err != nil {
			t.Fatal(err)
		}
		// the test's join keys are not really sealed, so every offer fails
		// with 500; this test checks only which tries get 429
		err = send(uncheckedClient(DefaultConnectTimeout), request, &BootstrapOffer{})
		switch {
		case try < triesPerSender && statusOf(err) == http.StatusTooManyRequests:
			t.Fatalf("try %d was refused under the limit", try+1)
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
