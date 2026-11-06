package agent

import (
	"cmp"
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"slices"
	"time"

	"github.com/webcodingcat/clowder/internal/api"
	"github.com/webcodingcat/clowder/internal/atomicfile"
	"github.com/webcodingcat/clowder/internal/buildinfo"
	"github.com/webcodingcat/clowder/internal/datakey"
	"github.com/webcodingcat/clowder/internal/hostrole"
	"github.com/webcodingcat/clowder/internal/jointoken"
	"github.com/webcodingcat/clowder/internal/network"
	"github.com/webcodingcat/clowder/internal/pki"
)

// JoinOptions are the values that cloud-init passes to clowderd join.
type JoinOptions struct {
	Token jointoken.Token
	// Via is an agent's address, optionally with a port.
	Via  string
	Name string
	// Roles are the roles the host takes, within what the token allows.
	Roles         hostrole.Set
	Underlay      netip.Addr
	PublicAddress netip.Addr
	ServerID      int64
	// RaftPort and APIPort are fixed in a cluster. Tests change them.
	RaftPort int
	APIPort  int
}

// Join makes this host a member of the cluster through the agent at
// options.Via. It refuses a host that already belongs to a cluster.
//
// The cluster uses up the token when it adds the host, so Join keeps its
// keys and then the answer in join.json. A later run continues from there
// without asking again, and writes the host as it joined, whatever options
// that run has.
func Join(ctx context.Context, files Files, options JoinOptions, hostNetwork HostNetwork, logger *slog.Logger) error {
	if _, err := os.Stat(files.raftDir()); err == nil {
		return fmt.Errorf("%s exists: this host was part of a cluster, or an init failed halfway "+
			"(to start anew, remove all of %s)", files.raftDir(), files.Dir)
	}
	if _, err := os.Stat(files.hostFile()); err == nil {
		return fmt.Errorf("%s exists: this host is part of a cluster", files.hostFile())
	}
	if !options.Underlay.Is4() || !options.PublicAddress.Is4() {
		return errors.New("the underlay and the public address must be IPv4 addresses")
	}
	if err := checkHostName(options.Name); err != nil {
		return err
	}
	joining, err := files.readJoining()
	if err != nil {
		return err
	}
	if joining.Answer == nil {
		if err := askToJoin(ctx, files, options, &joining); err != nil {
			return err
		}
		logger.Info("the cluster took this host", "host", joining.Answer.Host, "name", options.Name)
	}
	if err := writeJoined(ctx, files, joining, hostNetwork); err != nil {
		return err
	}
	if err := os.Remove(files.joinFile()); err != nil {
		return fmt.Errorf("remove %s, which holds the data key a second time: %w", files.joinFile(), err)
	}
	return nil
}

// joining is the content of join.json. Only root may read it, because it
// holds the host's keys and the data key.
type joining struct {
	HostKey      []byte          `json:"host_key"`      // PEM
	WireGuardKey []byte          `json:"wireguard_key"` // X25519, raw
	CA           string          `json:"ca_certificate,omitempty"`
	Answer       *api.JoinAnswer `json:"answer,omitempty"`
	Host         *Host           `json:"host,omitempty"`
}

// readJoining creates the keys and join.json if the file is missing.
func (f Files) readJoining() (joining, error) {
	var saved joining
	content, err := os.ReadFile(f.joinFile())
	if err == nil {
		if err := json.Unmarshal(content, &saved); err != nil {
			return joining{}, fmt.Errorf("read %s: %w", f.joinFile(), err)
		}
		return saved, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return joining{}, fmt.Errorf("read %s: %w", f.joinFile(), err)
	}
	hostKey, err := pki.NewKey()
	if err != nil {
		return joining{}, err
	}
	if saved.HostKey, err = pki.EncodeKey(hostKey); err != nil {
		return joining{}, err
	}
	wireGuardKey, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return joining{}, fmt.Errorf("make the WireGuard key: %w", err)
	}
	saved.WireGuardKey = wireGuardKey.Bytes()
	return saved, f.writeJoining(saved)
}

func (f Files) writeJoining(saved joining) error {
	content, err := json.Marshal(saved)
	if err != nil {
		return fmt.Errorf("write %s: %w", f.joinFile(), err)
	}
	return atomicfile.Write(f.joinFile(), content, 0o600)
}

func askToJoin(ctx context.Context, files Files, options JoinOptions, saved *joining) error {
	// clowderd join has no --connect-timeout: it asks one host, and nobody
	// waits at a terminal
	cluster, err := api.FindClusterByToken(ctx, options.Via, api.DefaultConnectTimeout, options.Token, buildinfo.Version)
	if err != nil {
		return err
	}
	hostKey, wireGuardKey, err := saved.keys()
	if err != nil {
		return err
	}
	answer, err := cluster.Join(ctx, api.JoinRequest{
		Name: options.Name, Roles: options.Roles, Underlay: options.Underlay, PublicAddress: options.PublicAddress,
		ServerID: options.ServerID, PublicKey: hostKey.Public().(ed25519.PublicKey),
		WGPublicKey: base64.StdEncoding.EncodeToString(wireGuardKey.PublicKey().Bytes()),
	})
	if err != nil {
		return err
	}
	saved.CA, saved.Answer = cluster.CAPEM, &answer
	saved.Host = &Host{
		ID: answer.Host, Name: options.Name, Roles: options.Roles, NodeAddress: network.NodeAddress(answer.Host),
		Underlay: options.Underlay, PublicAddress: options.PublicAddress, ServerID: options.ServerID,
		RaftPort: cmp.Or(options.RaftPort, RaftPort), APIPort: cmp.Or(options.APIPort, APIPort),
	}
	return files.writeJoining(*saved)
}

func (j joining) keys() (ed25519.PrivateKey, *ecdh.PrivateKey, error) {
	hostKey, err := pki.ParseKey(j.HostKey)
	if err != nil {
		return nil, nil, err
	}
	wireGuardKey, err := ecdh.X25519().NewPrivateKey(j.WireGuardKey)
	if err != nil {
		return nil, nil, fmt.Errorf("read the WireGuard key: %w", err)
	}
	return hostKey, wireGuardKey, nil
}

func writeJoined(ctx context.Context, files Files, saved joining, hostNetwork HostNetwork) error {
	answer, host := *saved.Answer, *saved.Host
	hostKey, wireGuardKey, err := saved.keys()
	if err != nil {
		return err
	}
	if err := checkJoinCertificate(saved.CA, answer, hostKey, host.Roles); err != nil {
		return err
	}
	dataKey, err := datakey.Parse(answer.DataKey)
	if err != nil {
		return err
	}
	if err := files.writeKeys([]byte(saved.CA), []byte(answer.Certificate), saved.HostKey, wireGuardKey, dataKey); err != nil {
		return err
	}
	if err := hostNetwork.SetUpInterface(ctx, host.NodeAddress); err != nil {
		return err
	}
	// the first peers come with the answer, not from Raft, because Raft
	// itself runs over the mesh to these peers
	mesh := network.Mesh{Self: host.ID}
	for _, peer := range answer.Peers {
		mesh.Peers = append(mesh.Peers, network.Peer{ID: peer.ID, PublicKey: peer.WGPublicKey, Underlay: peer.Underlay})
	}
	if err := files.writeMesh(mesh); err != nil {
		return err
	}
	// host.json comes last: a host with it has everything else
	return files.writeHost(host)
}

func checkJoinCertificate(caPEM string, answer api.JoinAnswer, hostKey ed25519.PrivateKey,
	roles hostrole.Set,
) error {
	ca, err := pki.ParseCertificate([]byte(caPEM))
	if err != nil {
		return err
	}
	certificate, err := pki.ParseCertificate([]byte(answer.Certificate))
	if err != nil {
		return err
	}
	if public, ok := certificate.PublicKey.(ed25519.PublicKey); !ok || !public.Equal(hostKey.Public()) {
		return errors.New("the cluster's certificate is not for this host's key")
	}
	identity, err := pki.Verify(ca, certificate, func(string) bool { return false }, time.Now())
	if err != nil {
		return fmt.Errorf("the cluster's certificate: %w", err)
	}
	if identity.Role != pki.RoleHost || identity.HostID != int(answer.Host) {
		return fmt.Errorf("the cluster's certificate is for %s, not for host %d", identity.Name, answer.Host)
	}
	if !slices.Equal(identity.HostRoles, roles) {
		return fmt.Errorf("the cluster's certificate names the roles %s, not %s", identity.HostRoles, roles)
	}
	return nil
}
