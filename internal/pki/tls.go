package pki

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"time"
)

// ClientTLSToHosts returns the TLS configuration of a caller (a CLI, or an
// agent calling another) that talks to a host. It trusts any host of the
// CA that is not revoked, whatever name or address it was reached by,
// because an endpoint may be a name and a host certificate lists only the
// host's addresses. revoked may be nil (the CLI does not know the list;
// the agent it reaches checks the CLI). With an empty certificate, the
// caller presents none, as on a joining host's first contact.
func ClientTLSToHosts(certificate tls.Certificate, ca *x509.Certificate, revoked func(serial string) bool) *tls.Config {
	var certificates []tls.Certificate
	if len(certificate.Certificate) > 0 {
		certificates = []tls.Certificate{certificate}
	}
	return &tls.Config{
		Certificates:       certificates,
		InsecureSkipVerify: true, // VerifyConnection checks the chain instead of the name
		VerifyConnection:   verifyHost(ca, revoked),
		MinVersion:         tls.VersionTLS13,
	}
}

// ServerTLSForHosts returns the TLS configuration of a server that only
// hosts may reach (Raft).
func ServerTLSForHosts(certificate tls.Certificate, ca *x509.Certificate, revoked func(serial string) bool) *tls.Config {
	return &tls.Config{
		Certificates:     []tls.Certificate{certificate},
		ClientAuth:       tls.RequireAnyClientCert,
		VerifyConnection: verifyHost(ca, revoked),
		MinVersion:       tls.VersionTLS13,
	}
}

// verifyHost checks that the other side presents a host certificate of the
// CA that is not revoked.
func verifyHost(ca *x509.Certificate, revoked func(serial string) bool) func(tls.ConnectionState) error {
	if revoked == nil {
		revoked = func(string) bool { return false }
	}
	return func(connection tls.ConnectionState) error {
		if len(connection.PeerCertificates) == 0 {
			return errors.New("the other side showed no certificate")
		}
		identity, err := Verify(ca, connection.PeerCertificates[0], revoked, time.Now())
		if err != nil {
			return err
		}
		if identity.Role != RoleHost {
			return fmt.Errorf("%s is no host of the cluster", identity.Name)
		}
		return nil
	}
}
