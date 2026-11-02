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

	"github.com/webcodingcat/clowder/internal/atomicfile"
	"github.com/webcodingcat/clowder/internal/datakey"
	"github.com/webcodingcat/clowder/internal/ingress"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/pki"
	"github.com/webcodingcat/clowder/internal/state"
)

// DefaultDir is where a host keeps its files.
const DefaultDir = "/var/lib/clowder"

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

// The storage driver's directories: the mount points of the volumes, and
// its state, beside which the agent keeps the time of the last trim.
func (f Files) volumesDir() string { return filepath.Join(f.Dir, "volumes") }
func (f Files) storageDir() string { return filepath.Join(f.Dir, "storage") }
func (f Files) lastTrim() string   { return filepath.Join(f.storageDir(), "last-trim") }

// The files of the stacks on this host, by their Compose project
// (names.Project): per stack its rendered Compose file and the LVs it
// mounts.
func (f Files) stacksDir() string              { return filepath.Join(f.Dir, "stacks") }
func (f Files) stackDir(project string) string { return filepath.Join(f.stacksDir(), project) }
func (f Files) stackFile(project string) string {
	return filepath.Join(f.stackDir(project), "compose.yaml")
}

func (f Files) stackVolumesFile(project string) string {
	return filepath.Join(f.stackDir(project), "volumes.json")
}
func (f Files) volumeRecordsDir() string { return filepath.Join(f.Dir, "volume-records") }
func (f Files) volumeRecord(volumeID string) string {
	return filepath.Join(f.volumeRecordsDir(), names.FileName(volumeID)+".json")
}

// The files of a stack's secrets, by its project: a file per secret a
// service asks for as a file (bind-mounted to /run/secrets/<name>), and the
// second Compose file with the secret variables.
func (f Files) secretsDir(project string) string { return filepath.Join(f.SecretsDir, project) }

func (f Files) secretFilesDir(project string) string {
	return filepath.Join(f.secretsDir(project), "files")
}

func (f Files) secretEnvFile(project string) string {
	return filepath.Join(f.secretsDir(project), "env.yaml")
}

func (f Files) secretFile(project, name string) string {
	return filepath.Join(f.secretFilesDir(project), name)
}

// Traefik's files sit beside stacks/, so the search for stacks that left
// this host never finds them: its Compose file, the directory of
// traefik.yml and the one of the routes, which Traefik watches.
func (f Files) traefikCompose() string    { return filepath.Join(f.Dir, "traefik", "compose.yaml") }
func (f Files) traefikStaticDir() string  { return filepath.Join(f.Dir, "traefik", "static") }
func (f Files) traefikDynamicDir() string { return filepath.Join(f.Dir, "traefik", "dynamic") }

// traefikCertificatesDir holds the routes' certificates and keys in clear,
// beside the stacks' secrets on the tmpfs: the "clowder-" prefix keeps a
// stack from taking it.
func (f Files) traefikCertificatesDir() string { return f.secretsDir(ingress.Project) }

// Host is host.json: who this host is in the cluster. init and join write it
// last, so a host with this file has all the others.
type Host struct {
	ID          state.HostID `json:"id"`
	Name        string       `json:"name"`
	NodeAddress netip.Addr   `json:"node_address"`
	Underlay    netip.Addr   `json:"underlay"`
	// PublicAddress is where users reach the API when the underlay is private.
	PublicAddress netip.Addr `json:"public_address,omitzero"`
	ServerRef     string     `json:"server_ref,omitempty"`
	RaftPort      int        `json:"raft_port"`
	APIPort       int        `json:"api_port"`
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
