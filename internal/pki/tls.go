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
// host of the CA that is not revoked, whatever name or address the caller
// used, because a host certificate lists only the host's addresses. revoked
// may be nil (no list known). With an empty certificate, the caller presents
// none.
func ClientTLSToHosts(certificate tls.Certificate, ca *x509.Certificate, revoked func(serial string) bool) *tls.Config {
	return clientTLS(certificate, verifyHost(ca, revoked, false))
}

// ClientTLSToControlPlane returns the TLS configuration of a caller that
// may only talk to a control-plane host: the CLI, a joining host, and
// clowderd's own commands. So a worker cannot pose as the cluster, not even
// a removed worker whose certificate has not expired yet. An endpoint may
// be a name, so the check uses the host's certificate, not the name.
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
// only control-plane hosts may reach. Two servers use it:
//   - Raft on every host, because only the leader and the candidates send;
//   - the API of a host without the control plane, because only the
//     control plane calls it.
func ServerTLSForControlPlane(certificate tls.Certificate, ca *x509.Certificate, revoked func(serial string) bool) *tls.Config {
	return &tls.Config{
		Certificates:     []tls.Certificate{certificate},
		ClientAuth:       tls.RequireAnyClientCert,
		VerifyConnection: verifyHost(ca, revoked, true),
		MinVersion:       tls.VersionTLS13,
	}
}

// verifyHost checks that the other side presents a host certificate of the
// CA that is not revoked. If controlPlane is set, the certificate must also
// name the role control-plane.
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
