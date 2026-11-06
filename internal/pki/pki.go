// Package pki is the cluster's certificate authority. Hosts and the CLI
// authenticate with the Ed25519 certificates that it issues (mutual TLS).
// A host certificate ends by revocation, not by expiry, and a CLI
// certificate lasts cliLifetime (docs/developing/cluster.md, Certificates).
package pki

import (
	"crypto"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/webcodingcat/clowder/internal/hostrole"
	"github.com/webcodingcat/clowder/internal/names"
)

const (
	caLifetime = 20 * 365 * 24 * time.Hour
	// leafLifetime is long, because a host that stays powered off for months
	// could not renew its certificate.
	leafLifetime = 10 * 365 * 24 * time.Hour
	// cliLifetime is kubeadm's default for client certificates.
	cliLifetime = 365 * 24 * time.Hour
	// clockSkew backdates certificates, so that a host whose clock runs a
	// little behind accepts them right away.
	clockSkew = time.Hour
)

// Role is what a certificate is for. It is stored as the certificate's
// organizational unit.
type Role string

// The roles.
const (
	RoleHost Role = "host"
	RoleCLI  Role = "cli"
)

// CA is the cluster's certificate authority.
type CA struct {
	Certificate *x509.Certificate
	key         ed25519.PrivateKey
}

// NewCA makes a CA for a cluster.
func NewCA(clusterName string, now time.Time) (*CA, error) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("make the CA key: %w", err)
	}
	template, err := newTemplate(pkix.Name{CommonName: "clowder CA " + clusterName}, now, caLifetime)
	if err != nil {
		return nil, err
	}
	template.IsCA = true
	template.BasicConstraintsValid = true
	template.MaxPathLenZero = true
	template.KeyUsage = x509.KeyUsageCertSign | x509.KeyUsageCRLSign
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		return nil, fmt.Errorf("make the CA certificate: %w", err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("make the CA certificate: %w", err)
	}
	return &CA{Certificate: certificate, key: private}, nil
}

// LoadCA reads a CA from its certificate and key in PEM.
func LoadCA(certificatePEM, keyPEM []byte) (*CA, error) {
	certificate, err := ParseCertificate(certificatePEM)
	if err != nil {
		return nil, err
	}
	key, err := ParseKey(keyPEM)
	if err != nil {
		return nil, err
	}
	if !certificate.IsCA {
		return nil, errors.New("the certificate is not a CA")
	}
	if public, ok := certificate.PublicKey.(ed25519.PublicKey); !ok || !public.Equal(key.Public()) {
		return nil, errors.New("the key does not belong to the CA certificate")
	}
	return &CA{Certificate: certificate, key: key}, nil
}

// CertificatePEM returns the CA certificate, for everyone who checks the
// cluster's certificates.
func (ca *CA) CertificatePEM() []byte {
	return encodeCertificate(ca.Certificate.Raw)
}

// KeyPEM returns the CA's private key. The cluster keeps it encrypted.
func (ca *CA) KeyPEM() ([]byte, error) {
	return EncodeKey(ca.key)
}

// IssueHost issues the certificate of a host, for TLS both as server and
// as client. It names the host's roles as its organizations, so that another
// host can read them from the certificate alone.
func (ca *CA) IssueHost(public crypto.PublicKey, id int, roles hostrole.Set, addresses []netip.Addr,
	now time.Time,
) ([]byte, error) {
	if err := roles.Check(); err != nil {
		return nil, fmt.Errorf("host %d: %w", id, err)
	}
	template, err := newTemplate(pkix.Name{
		CommonName:         HostName(id),
		OrganizationalUnit: []string{string(RoleHost)},
		Organization:       roles.Strings(),
	}, now, leafLifetime)
	if err != nil {
		return nil, err
	}
	template.KeyUsage = x509.KeyUsageDigitalSignature
	template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}
	template.DNSNames = []string{HostName(id)}
	for _, address := range addresses {
		template.IPAddresses = append(template.IPAddresses, net.IP(address.AsSlice()))
	}
	return ca.issue(template, public)
}

// DefaultCLIName is the name of a CLI certificate when its holder gave no
// name, like kubernetes-admin in kubeadm's admin.conf.
const DefaultCLIName = "admin"

// IssueCLI issues a certificate that the CLI presents. The name is only
// for the audit. It grants nothing, because every CLI certificate controls
// the cluster.
func (ca *CA) IssueCLI(public crypto.PublicKey, name string, now time.Time) ([]byte, error) {
	if !names.Valid(name) {
		return nil, fmt.Errorf("CLI name %q: %s", name, names.Rule)
	}
	template, err := newTemplate(pkix.Name{
		CommonName:         name,
		OrganizationalUnit: []string{string(RoleCLI)},
	}, now, cliLifetime)
	if err != nil {
		return nil, err
	}
	template.KeyUsage = x509.KeyUsageDigitalSignature
	template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	return ca.issue(template, public)
}

func (ca *CA) issue(template *x509.Certificate, public crypto.PublicKey) ([]byte, error) {
	if _, ok := public.(ed25519.PublicKey); !ok {
		return nil, fmt.Errorf("the public key is a %T, not Ed25519", public)
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.Certificate, public, ca.key)
	if err != nil {
		return nil, fmt.Errorf("issue %s: %w", template.Subject.CommonName, err)
	}
	return encodeCertificate(der), nil
}

// HostName returns the name in a host's certificate.
func HostName(id int) string {
	return "host-" + strconv.Itoa(id)
}

// Identity is who a verified certificate belongs to.
type Identity struct {
	Role Role
	// Name is the CLI's name, or the host's name (host-<id>).
	Name string
	// HostID and HostRoles are set for hosts.
	HostID    int
	HostRoles hostrole.Set
	Serial    string
	// NotBefore is when the certificate starts. The API uses it to check a
	// CLI certificate against the cluster's cutoff.
	NotBefore time.Time
}

// Verify checks a certificate that a peer presented against the CA, its
// lifetime and the revoked serials, and returns whose certificate it is.
func Verify(caCertificate, certificate *x509.Certificate, revoked func(serial string) bool, now time.Time) (Identity, error) {
	roots := x509.NewCertPool()
	roots.AddCert(caCertificate)
	_, err := certificate.Verify(x509.VerifyOptions{
		Roots:       roots,
		CurrentTime: now,
		KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
	if err != nil {
		return Identity{}, fmt.Errorf("certificate %s: %w", certificate.Subject.CommonName, err)
	}
	serial := Serial(certificate)
	if revoked(serial) {
		return Identity{}, fmt.Errorf("certificate %s (%s) is revoked", certificate.Subject.CommonName, serial)
	}
	identity := Identity{Name: certificate.Subject.CommonName, Serial: serial, NotBefore: certificate.NotBefore}
	if units := certificate.Subject.OrganizationalUnit; len(units) == 1 {
		identity.Role = Role(units[0])
	}
	switch identity.Role {
	case RoleCLI:
		if !names.Valid(identity.Name) {
			return Identity{}, fmt.Errorf("CLI certificate with the name %q", identity.Name)
		}
	case RoleHost:
		id, err := strconv.Atoi(strings.TrimPrefix(identity.Name, "host-"))
		if err != nil || HostName(id) != identity.Name {
			return Identity{}, fmt.Errorf("host certificate with the name %q", identity.Name)
		}
		roles, err := HostRoles(certificate)
		if err != nil {
			return Identity{}, err
		}
		identity.HostID, identity.HostRoles = id, roles
	default:
		return Identity{}, fmt.Errorf("certificate %s has no known role", identity.Name)
	}
	return identity, nil
}

// HostRoles reads the roles that a host certificate names, without
// verifying the certificate, because the agent compares them with its own
// files at its start, when its certificate may have expired.
func HostRoles(certificate *x509.Certificate) (hostrole.Set, error) {
	// DER sorts a SET OF by its encoding, so the organizations may come back
	// in a different order than IssueHost wrote them. New sorts them.
	named := make([]hostrole.Role, 0, len(certificate.Subject.Organization))
	for _, organization := range certificate.Subject.Organization {
		named = append(named, hostrole.Role(organization))
	}
	roles, err := hostrole.New(named...)
	if err != nil {
		return nil, fmt.Errorf("host certificate %s: %w", certificate.Subject.CommonName, err)
	}
	return roles, nil
}

// Serial returns a certificate's serial number in the form that the
// cluster lists it in.
func Serial(certificate *x509.Certificate) string {
	return certificate.SerialNumber.Text(16)
}

// NewKey makes a key pair for a host or the CLI.
func NewKey() (ed25519.PrivateKey, error) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("make a key: %w", err)
	}
	return private, nil
}

// EncodeKey writes a private key as PKCS #8 PEM.
func EncodeKey(key ed25519.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("encode the key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// ParseKey reads a private key written by EncodeKey.
func ParseKey(keyPEM []byte) (ed25519.PrivateKey, error) {
	block, _ := pem.Decode(keyPEM)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, errors.New("no private key in PEM")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("read the key: %w", err)
	}
	private, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("the key is a %T, not Ed25519", key)
	}
	return private, nil
}

// ParseCertificate reads one certificate in PEM.
func ParseCertificate(certificatePEM []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(certificatePEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("no certificate in PEM")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("read the certificate: %w", err)
	}
	return certificate, nil
}

// TrustProof binds the CA certificate to a proof key that the agent and
// the caller share (jointoken.ProofKey), so that a joining host can trust
// the CA on first contact.
func TrustProof(proofKey, caCertificatePEM []byte) []byte {
	mac := hmac.New(sha256.New, proofKey)
	mac.Write(caCertificatePEM)
	return mac.Sum(nil)
}

// CheckTrustProof tells whether a proof matches the CA certificate.
func CheckTrustProof(proofKey, caCertificatePEM, proof []byte) bool {
	return hmac.Equal(TrustProof(proofKey, caCertificatePEM), proof)
}

func newTemplate(subject pkix.Name, now time.Time, lifetime time.Duration) (*x509.Certificate, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("make a serial: %w", err)
	}
	return &x509.Certificate{
		SerialNumber: serial,
		Subject:      subject,
		NotBefore:    now.Add(-clockSkew),
		NotAfter:     now.Add(lifetime),
	}, nil
}

func encodeCertificate(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
