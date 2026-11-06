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

// DefaultSecretsDir holds the stacks' secrets in clear. /run is a tmpfs, so
// the secrets never reach a disk, and a boot clears them. It is not a
// systemd RuntimeDirectory: systemd removes that each time the agent stops,
// while the containers still use it.
const DefaultSecretsDir = "/run/clowder-secrets"

// The ports of an agent; tests use others.
const (
	RaftPort = 7400
	APIPort  = 7443
)

// Files are the paths of a host's files under one directory. Keys and
// certificates are readable by root only.
type Files struct {
	Dir string
	// SecretsDir holds the stacks' secrets in clear (DefaultSecretsDir, on
	// tmpfs); run needs it, init does not.
	SecretsDir string
}

// The files of a host.
func (f Files) hostFile() string        { return filepath.Join(f.Dir, "host.json") }
func (f Files) dataKey() string         { return filepath.Join(f.Dir, "keys", "data-key.txt") }
func (f Files) wireGuardKey() string    { return filepath.Join(f.Dir, "keys", "wireguard.key") }
func (f Files) caCertificate() string   { return filepath.Join(f.Dir, "pki", "ca.pem") }
func (f Files) hostCertificate() string { return filepath.Join(f.Dir, "pki", "host.pem") }
func (f Files) hostKey() string         { return filepath.Join(f.Dir, "pki", "host-key.pem") }
func (f Files) raftDir() string         { return filepath.Join(f.Dir, "raft") }
func (f Files) meshFile() string        { return filepath.Join(f.Dir, "net.json") }
func (f Files) joinFile() string        { return filepath.Join(f.Dir, "keys", "join.json") }

// The storage driver's directories: the mount points of the volumes, and
// the driver's state. The agent keeps the time of the last trim beside the
// state.
func (f Files) volumesDir() string { return filepath.Join(f.Dir, "volumes") }
func (f Files) storageDir() string { return filepath.Join(f.Dir, "storage") }
func (f Files) lastTrim() string   { return filepath.Join(f.storageDir(), "last-trim") }

// stackFiles are where the stack runner keeps the stacks on this host: a
// directory per stack, the secrets on the tmpfs.
func (f Files) stackFiles() stacks.Files {
	return stacks.Files{StacksDir: filepath.Join(f.Dir, "stacks"), SecretsDir: f.SecretsDir}
}

// volumeFiles are where the volume reconciler keeps the copies on this
// host: a record per volume, the time of the last trim.
func (f Files) volumeFiles() volumes.Files {
	return volumes.Files{RecordsDir: filepath.Join(f.Dir, "volume-records"), LastTrim: f.lastTrim()}
}

// Traefik's files sit beside stacks/, so the search for stacks that left
// this host never finds them. They are Traefik's Compose file, the
// directory of traefik.yml, and the directory of the routes, which Traefik
// watches.
func (f Files) traefikCompose() string    { return filepath.Join(f.Dir, "traefik", "compose.yaml") }
func (f Files) traefikStaticDir() string  { return filepath.Join(f.Dir, "traefik", "static") }
func (f Files) traefikDynamicDir() string { return filepath.Join(f.Dir, "traefik", "dynamic") }

// traefikCertificatesDir holds the routes' certificates and keys in clear,
// beside the stacks' secrets on the tmpfs. The "clowder-" prefix keeps a
// stack from using the same directory.
func (f Files) traefikCertificatesDir() string { return filepath.Join(f.SecretsDir, ingress.Project) }

// Host is host.json: who this host is in the cluster. init and join write it
// last, so a host with this file has all the others.
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

// checkControlPlane refuses a host command that only a control-plane host
// runs: one that hands out the cluster's keys or makes the host the only
// voter.
func (h Host) checkControlPlane(command string) error {
	if !h.Roles.Has(hostrole.ControlPlane) {
		return fmt.Errorf("this host has the roles %s; clowderd %s runs on a %s host only", h.Roles, command,
			hostrole.ControlPlane)
	}
	return nil
}

// raftAddress is where the host's Raft member listens and is reached.
func (h Host) raftAddress() string {
	return net.JoinHostPort(h.NodeAddress.String(), strconv.Itoa(h.RaftPort))
}

// apiAddresses are where the host serves the API: the underlay, the node
// address and the public address, each once.
func (h Host) apiAddresses() []string {
	var addresses []string
	for _, address := range []netip.Addr{h.Underlay, h.NodeAddress, h.PublicAddress} {
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

// credentials are what the host proves itself with.
type credentials struct {
	ca          *x509.Certificate
	certificate tls.Certificate
	dataKey     *datakey.Key
}

func (f Files) readCredentials() (credentials, error) {
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
	dataKey, err := datakey.Load(f.dataKey())
	if err != nil {
		return credentials{}, err
	}
	return credentials{ca: ca, certificate: certificate, dataKey: dataKey}, nil
}
