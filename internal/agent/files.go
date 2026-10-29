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
	"strconv"

	"github.com/webcodingcat/clowder/internal/atomicfile"
	"github.com/webcodingcat/clowder/internal/datakey"
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
}

// The files of a host.
func (f Files) hostFile() string        { return filepath.Join(f.Dir, "host.json") }
func (f Files) dataKey() string         { return filepath.Join(f.Dir, "keys", "data-key.txt") }
func (f Files) wireGuardKey() string    { return filepath.Join(f.Dir, "keys", "wireguard.key") }
func (f Files) caCertificate() string   { return filepath.Join(f.Dir, "pki", "ca.pem") }
func (f Files) hostCertificate() string { return filepath.Join(f.Dir, "pki", "host.pem") }
func (f Files) hostKey() string         { return filepath.Join(f.Dir, "pki", "host-key.pem") }
func (f Files) raftDir() string         { return filepath.Join(f.Dir, "raft") }

// Host is host.json: who this host is in the cluster. init and join write it
// last, so a host with this file has all the others.
type Host struct {
	ID          state.HostID `json:"id"`
	Name        string       `json:"name"`
	NodeAddress netip.Addr   `json:"node_address"`
	Underlay    netip.Addr   `json:"underlay"`
	RaftPort    int          `json:"raft_port"`
	APIPort     int          `json:"api_port"`
}

// raftAddress is where the host's Raft member listens and is reached.
func (h Host) raftAddress() string {
	return net.JoinHostPort(h.NodeAddress.String(), strconv.Itoa(h.RaftPort))
}

// apiAddresses are where the host serves the API: underlay and node address.
func (h Host) apiAddresses() []string {
	addresses := []string{net.JoinHostPort(h.Underlay.String(), strconv.Itoa(h.APIPort))}
	if h.NodeAddress != h.Underlay {
		addresses = append(addresses, net.JoinHostPort(h.NodeAddress.String(), strconv.Itoa(h.APIPort)))
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
		return err
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
