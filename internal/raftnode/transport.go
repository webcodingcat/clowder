package raftnode

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/hashicorp/raft"

	"github.com/webcodingcat/clowder/internal/pki"
)

// tlsStream is Raft's network layer over mutual TLS: both sides present a
// host certificate of the cluster CA that is not revoked. Users' and
// strangers' certificates fail the handshake.
type tlsStream struct {
	net.Listener
	advertise net.Addr
	client    *tls.Config
}

// newTLSStream listens on bind and tells peers to reach it at advertise.
func newTLSStream(bind string, advertise net.Addr, certificate tls.Certificate, ca *x509.Certificate,
	revoked func(serial string) bool,
) (*tlsStream, error) {
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	verifyHost := func(rawCertificates [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCertificates) == 0 {
			return errors.New("no certificate")
		}
		peer, err := x509.ParseCertificate(rawCertificates[0])
		if err != nil {
			return err
		}
		identity, err := pki.Verify(ca, peer, revoked, time.Now())
		if err != nil {
			return err
		}
		if identity.Role != pki.RoleHost {
			return fmt.Errorf("%s is no host", identity.Name)
		}
		return nil
	}
	server := &tls.Config{
		Certificates:          []tls.Certificate{certificate},
		ClientCAs:             roots,
		ClientAuth:            tls.RequireAndVerifyClientCert,
		VerifyPeerCertificate: verifyHost,
		MinVersion:            tls.VersionTLS13,
	}
	listener, err := tls.Listen("tcp", bind, server)
	if err != nil {
		return nil, fmt.Errorf("listen for Raft on %s: %w", bind, err)
	}
	client := &tls.Config{
		Certificates:          []tls.Certificate{certificate},
		RootCAs:               roots,
		VerifyPeerCertificate: verifyHost,
		MinVersion:            tls.VersionTLS13,
	}
	return &tlsStream{Listener: listener, advertise: advertise, client: client}, nil
}

// Addr is the address peers dial, not the one bound (which may be 0.0.0.0
// in a test).
func (s *tlsStream) Addr() net.Addr {
	return s.advertise
}

// Dial connects to a peer; its certificate must carry the dialed address.
func (s *tlsStream) Dial(address raft.ServerAddress, timeout time.Duration) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: timeout}
	connection, err := tls.DialWithDialer(dialer, "tcp", string(address), s.client)
	if err != nil {
		return nil, fmt.Errorf("dial Raft peer %s: %w", address, err)
	}
	return connection, nil
}
