package pki

import (
	"crypto/ed25519"
	"crypto/x509"
	"net/netip"
	"strings"
	"testing"
	"time"
)

var testTime = time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)

func notRevoked(string) bool { return false }

func newTestCA(t *testing.T) *CA {
	t.Helper()
	ca, err := NewCA("prod", testTime)
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

// issued returns a fresh key and the certificate issue makes for it.
func issued(t *testing.T, issue func(public ed25519.PublicKey) ([]byte, error)) (ed25519.PrivateKey, *x509.Certificate) {
	t.Helper()
	key, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	certificatePEM, err := issue(key.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := ParseCertificate(certificatePEM)
	if err != nil {
		t.Fatal(err)
	}
	return key, certificate
}

func TestVerifyTellsUsersAndHostsApart(t *testing.T) {
	ca := newTestCA(t)
	_, user := issued(t, func(public ed25519.PublicKey) ([]byte, error) {
		return ca.IssueUser(public, "alice", testTime)
	})
	_, host := issued(t, func(public ed25519.PublicKey) ([]byte, error) {
		return ca.IssueHost(public, 2, []netip.Addr{netip.MustParseAddr("10.77.0.3")}, testTime)
	})

	identity, err := Verify(ca.Certificate, user, notRevoked, testTime)
	if err != nil || identity.Role != RoleUser || identity.Name != "alice" || identity.Serial != Serial(user) {
		t.Errorf("user: %+v, %v", identity, err)
	}
	identity, err = Verify(ca.Certificate, host, notRevoked, testTime)
	if err != nil || identity.Role != RoleHost || identity.HostID != 2 || identity.Name != "host-2" {
		t.Errorf("host: %+v, %v", identity, err)
	}
	if err := host.VerifyHostname("10.77.0.3"); err != nil {
		t.Errorf("the host certificate is not valid for the host's address: %v", err)
	}
	if err := host.VerifyHostname("10.9.9.9"); err == nil {
		t.Error("the host certificate is valid for an address it does not have")
	}
}

func TestVerifyRefuses(t *testing.T) {
	ca := newTestCA(t)
	_, user := issued(t, func(public ed25519.PublicKey) ([]byte, error) {
		return ca.IssueUser(public, "alice", testTime)
	})
	otherCA := newTestCA(t)
	_, stranger := issued(t, func(public ed25519.PublicKey) ([]byte, error) {
		return otherCA.IssueUser(public, "mallory", testTime)
	})
	tests := []struct {
		name        string
		certificate *x509.Certificate
		revoked     func(string) bool
		at          time.Time
		want        string
	}{
		{"another CA", stranger, notRevoked, testTime, "unknown authority"},
		{"revoked", user, func(serial string) bool { return serial == Serial(user) }, testTime, "revoked"},
		{"expired", user, notRevoked, testTime.Add(leafLifetime + time.Hour), "expired"},
		{"slow clock within the skew", user, notRevoked, testTime.Add(-clockSkew / 2), ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Verify(ca.Certificate, test.certificate, test.revoked, test.at)
			switch {
			case test.want == "" && err != nil:
				t.Errorf("want no error, got %v", err)
			case test.want != "" && (err == nil || !strings.Contains(err.Error(), test.want)):
				t.Errorf("want an error with %q, got %v", test.want, err)
			}
		})
	}
}

func TestUserNamesFollowOneRule(t *testing.T) {
	ca := newTestCA(t)
	key, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", "a/b", "a,b", "Alice.Smith"} {
		if _, err := ca.IssueUser(key.Public(), name, testTime); err == nil {
			t.Errorf("%q: no error", name)
		}
	}
}

func TestTheCASurvivesPEM(t *testing.T) {
	ca := newTestCA(t)
	keyPEM, err := ca.KeyPEM()
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadCA(ca.CertificatePEM(), keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	_, user := issued(t, func(public ed25519.PublicKey) ([]byte, error) {
		return loaded.IssueUser(public, "alice", testTime)
	})
	if _, err := Verify(ca.Certificate, user, notRevoked, testTime); err != nil {
		t.Errorf("a certificate of the loaded CA does not verify with the original: %v", err)
	}

	otherKey, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	otherKeyPEM, err := EncodeKey(otherKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCA(ca.CertificatePEM(), otherKeyPEM); err == nil {
		t.Error("a CA with a foreign key loaded")
	}
}

func TestTrustProofNeedsTheBootstrapKey(t *testing.T) {
	ca := newTestCA(t)
	proof := TrustProof([]byte("bootstrap"), ca.CertificatePEM())
	if !CheckTrustProof([]byte("bootstrap"), ca.CertificatePEM(), proof) {
		t.Error("the proof does not check")
	}
	if CheckTrustProof([]byte("guess"), ca.CertificatePEM(), proof) {
		t.Error("the proof checks with another key")
	}
	if CheckTrustProof([]byte("bootstrap"), newTestCA(t).CertificatePEM(), proof) {
		t.Error("the proof checks for another CA")
	}
}
