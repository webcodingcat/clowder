package raftnode

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"time"

	"github.com/hashicorp/raft"

	"github.com/webcodingcat/clowder/internal/pki"
)

// tlsStream is Raft's network layer over mutual TLS. Both sides present a
// host certificate from the cluster CA that is not revoked. Certificates of
// users and of strangers fail the handshake.
type tlsStream struct {
	net.Listener
	advertise net.Addr
	client    *tls.Config
}

// newTLSStream listens on bind and tells peers to reach it at advertise.
func newTLSStream(bind string, advertise net.Addr, certificate tls.Certificate, ca *x509.Certificate,
	revoked func(serial string) bool,
) (*tlsStream, error) {
	listener, err := tls.Listen("tcp", bind, pki.ServerTLSForHosts(certificate, ca, revoked))
	if err != nil {
		return nil, fmt.Errorf("listen for Raft on %s: %w", bind, err)
	}
	client := pki.ClientTLSToHosts(certificate, ca, revoked)
	return &tlsStream{Listener: listener, advertise: advertise, client: client}, nil
}

// Addr is the address that peers dial, not the bound address (which may be
// 0.0.0.0 in a test).
func (s *tlsStream) Addr() net.Addr {
	return s.advertise
}

// Dial connects to a peer, which must show a host certificate of the cluster.
func (s *tlsStream) Dial(address raft.ServerAddress, timeout time.Duration) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: timeout}
	connection, err := tls.DialWithDialer(dialer, "tcp", string(address), s.client)
	if err != nil {
		return nil, fmt.Errorf("dial Raft peer %s: %w", address, err)
	}
	return connection, nil
}
