// Package hostfiles holds what clowderd's commands and its daemon share: a
// host's files under DefaultDir, its identity (host.json) and its
// credentials.
package hostfiles

import (
	"crypto/ecdh"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"

	"github.com/webcodingcat/clowder/internal/atomicfile"
	"github.com/webcodingcat/clowder/internal/datakey"
	"github.com/webcodingcat/clowder/internal/hostrole"
	"github.com/webcodingcat/clowder/internal/ingress"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/pki"
	"github.com/webcodingcat/clowder/internal/state"
)

// DefaultDir is where a host keeps its files.
const DefaultDir = "/var/lib/clowder"

// DefaultSecretsDir holds the stacks' secrets in plain text, on a tmpfs so
// that they never reach a disk. It is no systemd RuntimeDirectory, which
// systemd removes at every stop of the agent while containers use it.
const DefaultSecretsDir = "/run/clowder-secrets"

// The agent's ports. Tests use other ports.
const (
	RaftPort = 7400
	APIPort  = 7443
)

// Files are the paths of a host's files under one directory. Keys and
// certificates are readable by root only.
type Files struct {
	Dir string
	// SecretsDir holds the stacks' secrets and Traefik's certificates in
	// plain text; run needs it, the other commands do not. It is a field,
	// not a method like the other paths, because it lies outside Dir.
	SecretsDir string
}

// Default returns a host's files outside tests: under DefaultDir, with
// the secrets in DefaultSecretsDir.
func Default() Files {
	return Files{Dir: DefaultDir, SecretsDir: DefaultSecretsDir}
}

// HostFile is host.json.
func (f Files) HostFile() string { return filepath.Join(f.Dir, "host.json") }

// DataKey is the data key's file, on a control-plane host only.
func (f Files) DataKey() string { return filepath.Join(f.Dir, "keys", "data-key.txt") }

// WireGuardKey is the host's WireGuard key.
func (f Files) WireGuardKey() string { return filepath.Join(f.Dir, "keys", "wireguard.key") }

func (f Files) caCertificate() string   { return filepath.Join(f.Dir, "pki", "ca.pem") }
func (f Files) hostCertificate() string { return filepath.Join(f.Dir, "pki", "host.pem") }
func (f Files) hostKey() string         { return filepath.Join(f.Dir, "pki", "host-key.pem") }

// RaftDir holds the Raft log and its snapshots.
func (f Files) RaftDir() string { return filepath.Join(f.Dir, "raft") }

// RejoinMarker is there from a rejoin until the next start.
func (f Files) RejoinMarker() string { return filepath.Join(f.Dir, "rejoin") }

// MeshFile is net.json, the WireGuard mesh the host last applied.
func (f Files) MeshFile() string { return filepath.Join(f.Dir, "net.json") }

// JoinFile is join.json, which a join keeps until it wrote host.json.
func (f Files) JoinFile() string { return filepath.Join(f.Dir, "keys", "join.json") }

// VolumesDir holds the mount points of the volumes.
func (f Files) VolumesDir() string { return filepath.Join(f.Dir, "volumes") }

// StorageDir holds the storage driver's own files.
func (f Files) StorageDir() string { return filepath.Join(f.Dir, "storage") }

// LastTrim records the last weekly trim.
func (f Files) LastTrim() string { return filepath.Join(f.StorageDir(), "last-trim") }

// StacksDir holds a directory per stack on this host.
func (f Files) StacksDir() string { return filepath.Join(f.Dir, "stacks") }

// VolumeRecordsDir holds what the volume reconciler keeps per volume.
func (f Files) VolumeRecordsDir() string { return filepath.Join(f.Dir, "volume-records") }

// TraefikCompose is Traefik's Compose file. Traefik's files sit next to
// stacks/, not inside it, so the search for stacks that left this host
// never finds them.
func (f Files) TraefikCompose() string { return filepath.Join(f.Dir, "traefik", "compose.yaml") }

// TraefikStaticDir holds Traefik's static configuration.
func (f Files) TraefikStaticDir() string { return filepath.Join(f.Dir, "traefik", "static") }

// TraefikDynamicDir holds Traefik's routes.
func (f Files) TraefikDynamicDir() string { return filepath.Join(f.Dir, "traefik", "dynamic") }

// TraefikCertificatesDir holds the routes' certificates and keys in plain
// text, next to the stacks' secrets on the tmpfs. The "clowder-" prefix
// keeps a stack from using the same directory.
func (f Files) TraefikCertificatesDir() string { return filepath.Join(f.SecretsDir, ingress.Project) }

// Host is the content of host.json: this host's identity in the cluster.
// init and join write it last, so a host with this file has all the others.
type Host struct {
	ID   state.HostID `json:"id"`
	Name string       `json:"name"`
	// Roles are the host's roles, as its certificate names them.
	Roles       hostrole.Set `json:"roles"`
	NodeAddress netip.Addr   `json:"node_address"`
	Underlay    netip.Addr   `json:"underlay"`
	// PublicAddress is where users reach the API.
	PublicAddress netip.Addr `json:"public_address"`
	ServerID      int64      `json:"server_id"`
	RaftPort      int        `json:"raft_port"`
	APIPort       int        `json:"api_port"`
}

// CheckControlPlane guards the commands that hand out the cluster's keys or
// make the host the only voter.
func (h Host) CheckControlPlane(command string) error {
	if !h.Roles.Has(hostrole.ControlPlane) {
		return fmt.Errorf("this host has the roles %s; clowderd %s runs on a %s host only", h.Roles, command,
			hostrole.ControlPlane)
	}
	return nil
}

// RaftAddress is where the host's Raft member listens.
func (h Host) RaftAddress() string {
	return net.JoinHostPort(h.NodeAddress.String(), strconv.Itoa(h.RaftPort))
}

// CheckRoles makes sure that host.json, the host's certificate and the
// state name the same roles: the API trusts the certificate, but the loops
// read host.json and the state. A state that does not know the host yet
// (before the first entries of a join) is not compared.
func (h Host) CheckRoles(certificate *x509.Certificate, current *state.State) error {
	named, err := pki.HostRoles(certificate)
	if err != nil {
		return err
	}
	if !slices.Equal(named, h.Roles) {
		return fmt.Errorf("host.json names the roles %s, the host's certificate %s; set the host up anew", h.Roles, named)
	}
	if host, ok := current.Hosts[h.ID]; ok && !slices.Equal(host.Roles, h.Roles) {
		return fmt.Errorf("host.json names the roles %s, the cluster's state %s; set the host up anew", h.Roles, host.Roles)
	}
	return nil
}

// APIAddresses leaves out the public address on a host without the
// control-plane role: it serves only the control plane, over the private
// network.
func (h Host) APIAddresses() []string {
	reached := []netip.Addr{h.Underlay, h.NodeAddress}
	if h.Roles.Has(hostrole.ControlPlane) {
		reached = append(reached, h.PublicAddress)
	}
	var addresses []string
	for _, address := range reached {
		hostPort := net.JoinHostPort(address.String(), strconv.Itoa(h.APIPort))
		if address.IsValid() && !slices.Contains(addresses, hostPort) {
			addresses = append(addresses, hostPort)
		}
	}
	return addresses
}

// CheckHostName refuses a name that the state would refuse. The host's name
// in the cluster is its hostname, and a hostname may be an FQDN or contain
// capital letters, which the state does not accept.
func CheckHostName(name string) error {
	if !names.Valid(name) {
		return fmt.Errorf("the hostname %q cannot name a host of the cluster: %s", name, names.Rule)
	}
	return nil
}

// ReadHost reads host.json.
func (f Files) ReadHost() (Host, error) {
	content, err := os.ReadFile(f.HostFile())
	if err != nil {
		return Host{}, fmt.Errorf("read %s (is this host part of a cluster? clowderd init or join): %w",
			f.HostFile(), err)
	}
	var host Host
	if err := json.Unmarshal(content, &host); err != nil {
		return Host{}, fmt.Errorf("read %s: %w", f.HostFile(), err)
	}
	return host, nil
}

// WriteHost writes host.json.
func (f Files) WriteHost(host Host) error {
	content, err := json.MarshalIndent(host, "", "  ")
	if err != nil {
		return fmt.Errorf("write %s: %w", f.HostFile(), err)
	}
	return atomicfile.Write(f.HostFile(), append(content, '\n'), 0o600)
}

// Credentials are the host's keys and certificates.
type Credentials struct {
	CA          *x509.Certificate
	Certificate tls.Certificate
	// DataKey is nil on a host without the control-plane role
	DataKey *datakey.Key
}

// Decrypt opens a secret of the state with the data key. On a host without
// it, it fails: only the leader's loops and the API's routes for the CLI
// decrypt, and both run on the control plane alone.
func (c Credentials) Decrypt(encrypted state.Encrypted) ([]byte, error) {
	if c.DataKey == nil {
		return nil, fmt.Errorf("this host holds no data key; only %s hosts do", hostrole.ControlPlane)
	}
	return c.DataKey.Decrypt(encrypted)
}

// ReadCredentials reads the data key only on a control-plane host, the
// only one that holds it.
func (f Files) ReadCredentials(roles hostrole.Set) (Credentials, error) {
	caPEM, err := os.ReadFile(f.caCertificate())
	if err != nil {
		return Credentials{}, fmt.Errorf("read the CA certificate: %w", err)
	}
	ca, err := pki.ParseCertificate(caPEM)
	if err != nil {
		return Credentials{}, err
	}
	certificate, err := tls.LoadX509KeyPair(f.hostCertificate(), f.hostKey())
	if err != nil {
		return Credentials{}, fmt.Errorf("read the host certificate: %w", err)
	}
	read := Credentials{CA: ca, Certificate: certificate}
	if roles.Has(hostrole.ControlPlane) {
		if read.DataKey, err = datakey.Load(f.DataKey()); err != nil {
			return Credentials{}, err
		}
	}
	return read, nil
}

// WriteKeys is shared by init and join. dataKey is nil on a host without
// the control-plane role.
func (f Files) WriteKeys(caPEM, certificatePEM, hostKeyPEM []byte, wireGuardKey *ecdh.PrivateKey,
	dataKey *datakey.Key,
) error {
	writes := []struct {
		path    string
		content []byte
	}{
		{f.caCertificate(), caPEM},
		{f.hostCertificate(), certificatePEM},
		{f.hostKey(), hostKeyPEM},
		{f.WireGuardKey(), []byte(base64.StdEncoding.EncodeToString(wireGuardKey.Bytes()) + "\n")},
	}
	for _, write := range writes {
		if err := atomicfile.Write(write.path, write.content, 0o600); err != nil {
			return err
		}
	}
	if dataKey == nil {
		return nil
	}
	return dataKey.Save(f.DataKey())
}
