package pki

import (
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/webcodingcat/clowder/internal/hostrole"
)

// TLS checks against the real clock, so these tests issue against it too.

var loopback = []netip.Addr{netip.MustParseAddr("127.0.0.1")}

// hostAt issues a host certificate as of a time.
func hostAt(t *testing.T, ca *CA, id int, at time.Time) (ed25519.PrivateKey, *x509.Certificate) {
	t.Helper()
	return issued(t, func(public ed25519.PublicKey) ([]byte, error) {
		return ca.IssueHost(public, id, hostrole.DefaultInit, loopback, at)
	})
}

// tlsCertificate is a key and its certificate as TLS shows them.
func tlsCertificate(key ed25519.PrivateKey, certificate *x509.Certificate) tls.Certificate {
	return tls.Certificate{Certificate: [][]byte{certificate.Raw}, PrivateKey: key, Leaf: certificate}
}

func TestTheTLSCheckLetsOnlyHostsOfTheCAThrough(t *testing.T) {
	ca, err := NewCA("prod", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	_, host := hostAt(t, ca, 1, time.Now())
	_, revokedHost := hostAt(t, ca, 2, time.Now())
	_, expired := hostAt(t, ca, 3, time.Now().Add(-leafLifetime-time.Hour))
	_, notYetValid := hostAt(t, ca, 4, time.Now().Add(2*clockSkew))
	_, user := issued(t, func(public ed25519.PublicKey) ([]byte, error) {
		return ca.IssueCLI(public, "alice", time.Now())
	})
	otherCA, err := NewCA("prod", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	_, stranger := hostAt(t, otherCA, 1, time.Now())
	revoked := func(serial string) bool { return serial == Serial(revokedHost) }

	tests := []struct {
		name  string
		chain []*x509.Certificate
		want  string // "" for no error
	}{
		{"a host", []*x509.Certificate{host}, ""},
		{"no certificate", nil, "no certificate"},
		{"a user", []*x509.Certificate{user}, "no host"},
		{"a host of another CA", []*x509.Certificate{stranger}, "unknown authority"},
		// TLS proves the key of the first certificate only
		{"a host's certificate behind a stranger's", []*x509.Certificate{stranger, host}, "unknown authority"},
		{"a revoked host", []*x509.Certificate{revokedHost}, "revoked"},
		// x509 says "expired or is not yet valid" to both; the detail tells
		{"an expired host", []*x509.Certificate{expired}, "is after"},
		{"a host not valid yet", []*x509.Certificate{notYetValid}, "is before"},
	}
	configs := map[string]*tls.Config{
		"client": ClientTLSToHosts(tls.Certificate{}, ca.Certificate, revoked),
		"server": ServerTLSForHosts(tls.Certificate{}, ca.Certificate, revoked),
	}
	for side, config := range configs {
		for _, test := range tests {
			t.Run(side+": "+test.name, func(t *testing.T) {
				err := config.VerifyConnection(tls.ConnectionState{PeerCertificates: test.chain})
				switch {
				case test.want == "" && err != nil:
					t.Errorf("want no error, got %v", err)
				case test.want != "" && (err == nil || !strings.Contains(err.Error(), test.want)):
					t.Errorf("want an error with %q, got %v", test.want, err)
				}
			})
		}
	}

	// the CLI does not know the revoked serials
	withoutList := ClientTLSToHosts(tls.Certificate{}, ca.Certificate, nil)
	if err := withoutList.VerifyConnection(tls.ConnectionState{PeerCertificates: []*x509.Certificate{revokedHost}}); err != nil {
		t.Errorf("without a list of revoked serials: %v", err)
	}
}

// handshake runs a TLS handshake between a server and a client on
// 127.0.0.1 and returns what each side said.
func handshake(t *testing.T, server, client *tls.Config) (serverErr, clientErr error) {
	t.Helper()
	listener, err := tls.Listen("tcp", "127.0.0.1:0", server)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	served := make(chan error, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			served <- err
			return
		}
		defer func() { _ = connection.Close() }()
		served <- connection.(*tls.Conn).Handshake()
	}()
	connection, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()
	clientConnection := tls.Client(connection, client)
	clientErr = clientConnection.Handshake()
	if clientErr == nil {
		// in TLS 1.3 the server checks the client's certificate after the
		// client finished; its refusal comes with the first read
		_ = clientConnection.SetReadDeadline(time.Now().Add(time.Second))
		_, _ = clientConnection.Read(make([]byte, 1))
	}
	return <-served, clientErr
}

func TestHostsTalkThroughTheTLSConfigsAndOthersDoNot(t *testing.T) {
	ca, err := NewCA("prod", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	host := func(id int) tls.Certificate {
		return tlsCertificate(hostAt(t, ca, id, time.Now()))
	}
	server := ServerTLSForHosts(host(0), ca.Certificate, nil)

	if serverErr, clientErr := handshake(t, server, ClientTLSToHosts(host(1), ca.Certificate, nil)); serverErr != nil || clientErr != nil {
		t.Errorf("two hosts: server %v, client %v", serverErr, clientErr)
	}

	user := tlsCertificate(issued(t, func(public ed25519.PublicKey) ([]byte, error) {
		return ca.IssueCLI(public, "alice", time.Now())
	}))
	otherCA, err := NewCA("prod", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	stranger := tlsCertificate(hostAt(t, otherCA, 1, time.Now()))
	for name, certificate := range map[string]tls.Certificate{
		"a user":               user,
		"a host of another CA": stranger,
		"no certificate":       {},
	} {
		serverErr, _ := handshake(t, server, ClientTLSToHosts(certificate, ca.Certificate, nil))
		if serverErr == nil {
			t.Errorf("the server let %s in", name)
		}
	}

	// the other way round: a host's client refuses a server of another CA
	strangeServer := ServerTLSForHosts(stranger, otherCA.Certificate, nil)
	if _, clientErr := handshake(t, strangeServer, ClientTLSToHosts(host(1), ca.Certificate, nil)); clientErr == nil ||
		!strings.Contains(clientErr.Error(), "unknown authority") {
		t.Errorf("the client took a server of another CA: %v", clientErr)
	}
}
