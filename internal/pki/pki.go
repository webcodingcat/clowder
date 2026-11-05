// Package pki is the cluster's certificate authority. Hosts and the CLI
// authenticate with certificates it issues (mTLS): hosts between agents and
// for Raft, the CLI towards the agents. Keys are Ed25519. Hosts and the
// bootstrap's CLI send only their public key; clowderd credentials makes
// the CLI's key on a host and hands it over with the certificate, as
// kubeadm does with admin.conf.
//
// A host certificate ends by revocation, not by expiry: the cluster keeps
// the revoked serials, and Verify takes them. CLI certificates end with
// the cluster's cutoff (state.SetCLICutoff), which the API checks. The
// long lifetimes spare hosts that stay powered off for months a renewal
// they cannot do.
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
	"os"
	"strconv"
	"strings"
	"time"
)

// Lifetimes of the certificates.
const (
	caLifetime   = 20 * 365 * 24 * time.Hour
	leafLifetime = 10 * 365 * 24 * time.Hour
	// clockSkew backdates certificates, so a host whose clock runs a little
	// behind accepts them at once.
	clockSkew = time.Hour
)

// Role is what a certificate is for; it is the certificate's organizational
// unit.
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

// CertificatePEM is the CA certificate, for everyone who checks the cluster's
// certificates.
func (ca *CA) CertificatePEM() []byte {
	return encodeCertificate(ca.Certificate.Raw)
}

// KeyPEM is the CA's private key; the cluster keeps it sealed.
func (ca *CA) KeyPEM() ([]byte, error) {
	return EncodeKey(ca.key)
}

// IssueHost issues the certificate of a host: for TLS both as server and as
// client, valid for its addresses (underlay and node address).
func (ca *CA) IssueHost(public crypto.PublicKey, id int, addresses []netip.Addr, now time.Time) ([]byte, error) {
	template, err := newTemplate(pkix.Name{
		CommonName:         HostName(id),
		OrganizationalUnit: []string{string(RoleHost)},
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

// CLIName is the name in every CLI certificate: the cluster has no users,
// only the CLI.
const CLIName = "cli"

// IssueCLI issues a certificate the CLI presents.
func (ca *CA) IssueCLI(public crypto.PublicKey, now time.Time) ([]byte, error) {
	template, err := newTemplate(pkix.Name{
		CommonName:         CLIName,
		OrganizationalUnit: []string{string(RoleCLI)},
	}, now, leafLifetime)
	if err != nil {
		return nil, err
	}
	template.KeyUsage = x509.KeyUsageDigitalSignature
	template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	return ca.issue(template, public)
}

// issue signs a template for a public key and returns the certificate in PEM.
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

// HostName is the name in a host's certificate.
func HostName(id int) string {
	return "host-" + strconv.Itoa(id)
}

// Identity is who a verified certificate belongs to.
type Identity struct {
	Role Role
	// Name is CLIName, or the host's name (host-<id>).
	Name string
	// HostID is set for hosts.
	HostID int
	Serial string
	// NotBefore is when the certificate starts: the API holds a CLI
	// certificate against the cluster's cutoff with it.
	NotBefore time.Time
}

// Verify checks a certificate a peer presented (a CLI, or an agent calling
// another): issued by the CA, in its lifetime and not revoked. It tells
// whose it is. It needs only the CA certificate, not its key.
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
		if identity.Name != CLIName {
			return Identity{}, fmt.Errorf("CLI certificate with the name %q", identity.Name)
		}
	case RoleHost:
		id, err := strconv.Atoi(strings.TrimPrefix(identity.Name, "host-"))
		if err != nil || HostName(id) != identity.Name {
			return Identity{}, fmt.Errorf("host certificate with the name %q", identity.Name)
		}
		identity.HostID = id
	default:
		return Identity{}, fmt.Errorf("certificate %s has no known role", identity.Name)
	}
	return identity, nil
}

// Serial is a certificate's serial number as the cluster lists it.
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

// TrustProof binds the CA certificate to the bootstrap key. The CLI and a
// joining host have no CA to trust yet on first contact; the agent sends its
// CA certificate with this proof, and only someone holding the bootstrap key
// can make it.
func TrustProof(bootstrapKey, caCertificatePEM []byte) []byte {
	mac := hmac.New(sha256.New, bootstrapKey)
	mac.Write(caCertificatePEM)
	return mac.Sum(nil)
}

// MinBootstrapKeyLength is the shortest bootstrap key taken. The key is the
// only defense of the bootstrap until the CLI has its certificate, and the
// offer's proof lets anyone try keys offline; OpenTofu's example makes 48
// letters and digits, and a key chosen by hand must not be much weaker.
const MinBootstrapKeyLength = 32

// ReadBootstrapKey reads the bootstrap key from a file, without the line end
// an editor or echo adds. clowderd init and clowder cluster bootstrap take
// it from a file, never from the command line, where /proc and shell
// histories would keep it.
func ReadBootstrapKey(path string) (string, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read the bootstrap key: %w", err)
	}
	key := strings.TrimSpace(string(content))
	if len(key) < MinBootstrapKeyLength {
		return "", fmt.Errorf("read the bootstrap key: %s holds %d characters, fewer than %d",
			path, len(key), MinBootstrapKeyLength)
	}
	return key, nil
}

// CheckTrustProof tells whether a proof matches the CA certificate.
func CheckTrustProof(bootstrapKey, caCertificatePEM, proof []byte) bool {
	return hmac.Equal(TrustProof(bootstrapKey, caCertificatePEM), proof)
}

// newTemplate fills in what every certificate has: a random serial and the
// lifetime.
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
