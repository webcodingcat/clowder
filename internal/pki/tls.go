package pki

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"time"

	"github.com/webcodingcat/clowder/internal/hostrole"
)

// ClientTLSToHosts returns the TLS configuration of an agent that talks to
// another host of any role, as the leader does with a worker. It trusts any
// host of the CA that is not revoked, whatever name or address it was
// reached by, because a host certificate lists only the host's addresses.
// revoked may be nil (no list known). With an empty certificate, the caller
// presents none.
func ClientTLSToHosts(certificate tls.Certificate, ca *x509.Certificate, revoked func(serial string) bool) *tls.Config {
	return clientTLS(certificate, verifyHost(ca, revoked, false))
}

// ClientTLSToControlPlane returns the TLS configuration of a caller that
// may only talk to a control-plane host: the CLI, a joining host, and
// clowderd's own commands. A worker, even a removed one whose certificate
// has not expired yet, cannot answer it as the cluster. An endpoint may be
// a name, so the host is checked by its certificate, not by the name.
func ClientTLSToControlPlane(certificate tls.Certificate, ca *x509.Certificate, revoked func(serial string) bool) *tls.Config {
	return clientTLS(certificate, verifyHost(ca, revoked, true))
}

func clientTLS(certificate tls.Certificate, verify func(tls.ConnectionState) error) *tls.Config {
	var certificates []tls.Certificate
	if len(certificate.Certificate) > 0 {
		certificates = []tls.Certificate{certificate}
	}
	return &tls.Config{
		Certificates:       certificates,
		InsecureSkipVerify: true, // VerifyConnection checks the chain instead of the name
		VerifyConnection:   verify,
		MinVersion:         tls.VersionTLS13,
	}
}

// ServerTLSForControlPlane returns the TLS configuration of a server that
// only control-plane hosts may reach: Raft on every host, where only the
// leader and the candidates send, and the API of a host without the
// control plane, which only the control plane calls.
func ServerTLSForControlPlane(certificate tls.Certificate, ca *x509.Certificate, revoked func(serial string) bool) *tls.Config {
	return &tls.Config{
		Certificates:     []tls.Certificate{certificate},
		ClientAuth:       tls.RequireAnyClientCert,
		VerifyConnection: verifyHost(ca, revoked, true),
		MinVersion:       tls.VersionTLS13,
	}
}

// verifyHost checks that the other side presents a host certificate of the
// CA that is not revoked and, if controlPlane is set, names the role
// control-plane.
func verifyHost(ca *x509.Certificate, revoked func(serial string) bool, controlPlane bool) func(tls.ConnectionState) error {
	if revoked == nil {
		revoked = func(string) bool { return false }
	}
	return func(connection tls.ConnectionState) error {
		if len(connection.PeerCertificates) == 0 {
			return errors.New("the other side showed no certificate")
		}
		identity, err := Verify(ca, connection.PeerCertificates[0], revoked, time.Now())
		switch {
		case err != nil:
			return err
		case identity.Role != RoleHost:
			return fmt.Errorf("%s is no host of the cluster", identity.Name)
		case controlPlane && !identity.HostRoles.Has(hostrole.ControlPlane):
			return fmt.Errorf("%s is no %s host (its roles are %s)", identity.Name, hostrole.ControlPlane, identity.HostRoles)
		}
		return nil
	}
}
