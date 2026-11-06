package agent

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"

	"github.com/webcodingcat/clowder/internal/agent/stacks"
	"github.com/webcodingcat/clowder/internal/agent/volumes"
	"github.com/webcodingcat/clowder/internal/atomicfile"
	"github.com/webcodingcat/clowder/internal/datakey"
	"github.com/webcodingcat/clowder/internal/hostrole"
	"github.com/webcodingcat/clowder/internal/ingress"
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
	// SecretsDir is DefaultSecretsDir outside tests. run needs it, init
	// does not.
	SecretsDir string
}

func (f Files) hostFile() string        { return filepath.Join(f.Dir, "host.json") }
func (f Files) dataKey() string         { return filepath.Join(f.Dir, "keys", "data-key.txt") }
func (f Files) wireGuardKey() string    { return filepath.Join(f.Dir, "keys", "wireguard.key") }
func (f Files) caCertificate() string   { return filepath.Join(f.Dir, "pki", "ca.pem") }
func (f Files) hostCertificate() string { return filepath.Join(f.Dir, "pki", "host.pem") }
func (f Files) hostKey() string         { return filepath.Join(f.Dir, "pki", "host-key.pem") }
func (f Files) raftDir() string         { return filepath.Join(f.Dir, "raft") }
func (f Files) rejoinMarker() string    { return filepath.Join(f.Dir, "rejoin") }
func (f Files) meshFile() string        { return filepath.Join(f.Dir, "net.json") }
func (f Files) joinFile() string        { return filepath.Join(f.Dir, "keys", "join.json") }

func (f Files) volumesDir() string { return filepath.Join(f.Dir, "volumes") }
func (f Files) storageDir() string { return filepath.Join(f.Dir, "storage") }
func (f Files) lastTrim() string   { return filepath.Join(f.storageDir(), "last-trim") }

func (f Files) stackFiles() stacks.Files {
	return stacks.Files{StacksDir: filepath.Join(f.Dir, "stacks"), SecretsDir: f.SecretsDir}
}

func (f Files) volumeFiles() volumes.Files {
	return volumes.Files{RecordsDir: filepath.Join(f.Dir, "volume-records"), LastTrim: f.lastTrim()}
}

// Traefik's files sit next to stacks/, not inside it, so the search for
// stacks that left this host never finds them.
func (f Files) traefikCompose() string    { return filepath.Join(f.Dir, "traefik", "compose.yaml") }
func (f Files) traefikStaticDir() string  { return filepath.Join(f.Dir, "traefik", "static") }
func (f Files) traefikDynamicDir() string { return filepath.Join(f.Dir, "traefik", "dynamic") }

// traefikCertificatesDir holds the routes' certificates and keys in plain
// text, next to the stacks' secrets on the tmpfs. The "clowder-" prefix
// keeps a stack from using the same directory.
func (f Files) traefikCertificatesDir() string { return filepath.Join(f.SecretsDir, ingress.Project) }

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

// checkControlPlane guards the commands that hand out the cluster's keys or
// make the host the only voter.
func (h Host) checkControlPlane(command string) error {
	if !h.Roles.Has(hostrole.ControlPlane) {
		return fmt.Errorf("this host has the roles %s; clowderd %s runs on a %s host only", h.Roles, command,
			hostrole.ControlPlane)
	}
	return nil
}

func (h Host) raftAddress() string {
	return net.JoinHostPort(h.NodeAddress.String(), strconv.Itoa(h.RaftPort))
}

// checkRoles makes sure that host.json, the host's certificate and the
// state name the same roles: the API trusts the certificate, but the loops
// read host.json and the state. A state that does not know the host yet
// (before the first entries of a join) is not compared.
func (h Host) checkRoles(certificate *x509.Certificate, current *state.State) error {
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

// apiAddresses leaves out the public address on a host without the
// control-plane role: it serves only the control plane, over the private
// network.
func (h Host) apiAddresses() []string {
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

func (f Files) readHost() (Host, error) {
	content, err := os.ReadFile(f.hostFile())
	if err != nil {
		return Host{}, fmt.Errorf("read %s (is this host part of a cluster? clowderd init or join): %w",
			f.hostFile(), err)
	}
	var host Host
	if err := json.Unmarshal(content, &host); err != nil {
		return Host{}, fmt.Errorf("read %s: %w", f.hostFile(), err)
	}
	return host, nil
}

func (f Files) writeHost(host Host) error {
	content, err := json.MarshalIndent(host, "", "  ")
	if err != nil {
		return fmt.Errorf("write %s: %w", f.hostFile(), err)
	}
	return atomicfile.Write(f.hostFile(), append(content, '\n'), 0o600)
}

type credentials struct {
	ca          *x509.Certificate
	certificate tls.Certificate
	// dataKey is nil on a host without the control-plane role
	dataKey *datakey.Key
}

// decrypt opens a secret of the state with the data key. On a host without
// it, it fails: only the leader's loops and the API's routes for the CLI
// decrypt, and both run on the control plane alone.
func (c credentials) decrypt(encrypted state.Encrypted) ([]byte, error) {
	if c.dataKey == nil {
		return nil, fmt.Errorf("this host holds no data key; only %s hosts do", hostrole.ControlPlane)
	}
	return c.dataKey.Decrypt(encrypted)
}

// readCredentials reads the data key only on a control-plane host, the
// only one that holds it.
func (f Files) readCredentials(roles hostrole.Set) (credentials, error) {
	caPEM, err := os.ReadFile(f.caCertificate())
	if err != nil {
		return credentials{}, fmt.Errorf("read the CA certificate: %w", err)
	}
	ca, err := pki.ParseCertificate(caPEM)
	if err != nil {
		return credentials{}, err
	}
	certificate, err := tls.LoadX509KeyPair(f.hostCertificate(), f.hostKey())
	if err != nil {
		return credentials{}, fmt.Errorf("read the host certificate: %w", err)
	}
	read := credentials{ca: ca, certificate: certificate}
	if roles.Has(hostrole.ControlPlane) {
		if read.dataKey, err = datakey.Load(f.dataKey()); err != nil {
			return credentials{}, err
		}
	}
	return read, nil
}
