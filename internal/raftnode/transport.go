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
// host certificate from the cluster CA that is not revoked.
//   - The listener accepts only control-plane hosts. Only the leader and
//     the candidates send, and they are voters. A worker that got in could
//     send entries of its own and bypass every check of the API.
//   - Dial accepts any host, because the leader also sends to the hosts
//     that follow the log without a vote.
//
// CLI certificates and certificates from other CAs fail the handshake.
type tlsStream struct {
	net.Listener
	advertise net.Addr
	client    *tls.Config
}

func newTLSStream(bind string, advertise net.Addr, certificate tls.Certificate, ca *x509.Certificate,
	revoked func(serial string) bool,
) (*tlsStream, error) {
	listener, err := tls.Listen("tcp", bind, pki.ServerTLSForControlPlane(certificate, ca, revoked))
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

func (s *tlsStream) Dial(address raft.ServerAddress, timeout time.Duration) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: timeout}
	connection, err := tls.DialWithDialer(dialer, "tcp", string(address), s.client)
	if err != nil {
		return nil, fmt.Errorf("dial Raft peer %s: %w", address, err)
	}
	return connection, nil
}
