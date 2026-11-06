// Package initialize is clowderd init: it makes a host the first of a new
// cluster. (init cannot name a Go package.)
package initialize

import (
	"cmp"
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"time"

	"filippo.io/age"
	"github.com/spf13/cobra"

	"github.com/webcodingcat/clowder/internal/clowderd/hostfiles"
	"github.com/webcodingcat/clowder/internal/clowderd/hostsetup"
	"github.com/webcodingcat/clowder/internal/datakey"
	"github.com/webcodingcat/clowder/internal/hostrole"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/network"
	"github.com/webcodingcat/clowder/internal/pki"
	"github.com/webcodingcat/clowder/internal/raftnode"
	"github.com/webcodingcat/clowder/internal/state"
)

// DefaultClusterName is the cluster's name when init is given none, as
// kubeadm's clusterName is "kubernetes" by default.
const DefaultClusterName = "clowder-cluster"

// Options are the values that cloud-init passes to clowderd init.
type Options struct {
	// ClusterName is the cluster's name, like kubeadm's clusterName. It is
	// only a label: no check depends on it.
	ClusterName string
	Name        string
	// Roles are the host's roles. The first host must be a control-plane
	// host.
	Roles         hostrole.Set
	Underlay      netip.Addr
	PublicAddress netip.Addr
	// ServerID is the Hetzner Cloud ID of the server, for the Floating IP
	// and fencing.
	ServerID int64
	// NodeAddress, RaftPort and APIPort are fixed in a cluster. Tests change
	// them to run on 127.0.0.1.
	NodeAddress netip.Addr
	RaftPort    int
	APIPort     int
}

// Network sets up the host's interface clowder0 (network.Host, or a fake
// in tests).
type Network interface {
	SetUpInterface(ctx context.Context, nodeAddress netip.Addr) error
}

const leaderTimeout = 30 * time.Second

// NewCommand returns clowderd init, which sets up the host's files and its
// interface through hostNetwork.
func NewCommand(files hostfiles.Files, hostNetwork Network) *cobra.Command {
	var flags hostsetup.HostFlags
	var clusterName string
	initCommand := &cobra.Command{
		Use:   "init",
		Short: "Make this host the first of a new cluster (from cloud-init)",
		Long: "Makes this host the first of a new cluster. The first credentials come from root on it\n" +
			"afterwards: ssh root@<host> clowderd credentials | clowder config import -\n" +
			"Then export the cluster's sealing key and keep it offline: clowderd sealing-key export",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			host, err := flags.Parse()
			if err != nil {
				return err
			}
			options := Options{
				ClusterName: clusterName, Name: host.Name, Roles: host.Roles, Underlay: host.Underlay,
				PublicAddress: host.PublicAddress, ServerID: host.ServerID,
			}
			if err := Init(cmd.Context(), files, options, hostNetwork, slog.Default()); err != nil {
				return err
			}
			return flags.StartService(cmd.Context())
		},
	}
	flags.Add(initCommand, hostrole.DefaultInit)
	initCommand.Flags().StringVar(&clusterName, "cluster-name", DefaultClusterName,
		"the cluster's name: cluster status shows it, alerts and new credentials carry it")
	return initCommand
}

// Init makes this host the first of a new cluster. It refuses a host that
// already belongs to a cluster.
func Init(ctx context.Context, files hostfiles.Files, options Options, hostNetwork Network, logger *slog.Logger) error {
	if _, err := os.Stat(files.RaftDir()); err == nil {
		return fmt.Errorf("%s exists: this host was part of a cluster, or an init failed halfway "+
			"(to start anew, remove all of %s)", files.RaftDir(), files.Dir)
	}
	if options.Name == "" || options.ServerID <= 0 {
		return errors.New("init needs the host's name and its server ID")
	}
	// The checks below repeat what the state refuses. If the state refused
	// a value instead, init would stop halfway.
	// a DNS label only (applyBootstrap)
	if !names.Valid(options.ClusterName) {
		return fmt.Errorf("cluster name %q: %s", options.ClusterName, names.Rule)
	}
	// a DNS label only (checkNewHost)
	if err := hostfiles.CheckHostName(options.Name); err != nil {
		return err
	}
	// a control-plane host only (applyBootstrap)
	if !options.Roles.Has(hostrole.ControlPlane) {
		return fmt.Errorf("the roles %s lack %s: the first host is the cluster's only voter", options.Roles,
			hostrole.ControlPlane)
	}
	// IPv4 only (checkNewHost)
	if !options.Underlay.Is4() || !options.PublicAddress.Is4() {
		return errors.New("the underlay and the public address must be IPv4 addresses")
	}
	host := hostfiles.Host{
		ID: 0, Name: options.Name, Roles: options.Roles, NodeAddress: options.NodeAddress, Underlay: options.Underlay,
		PublicAddress: options.PublicAddress, ServerID: options.ServerID,
		RaftPort: options.RaftPort, APIPort: options.APIPort,
	}
	if !host.NodeAddress.IsValid() {
		host.NodeAddress = network.NodeAddress(host.ID)
	}
	host.RaftPort = cmp.Or(host.RaftPort, hostfiles.RaftPort)
	host.APIPort = cmp.Or(host.APIPort, hostfiles.APIPort)
	keys, hostEntry, err := createKeys(files, options.ClusterName, host)
	if err != nil {
		return err
	}
	if err := hostNetwork.SetUpInterface(ctx, host.NodeAddress); err != nil {
		return err
	}
	if err := bootstrapRaft(ctx, files, host, keys, hostEntry, options.ClusterName, logger); err != nil {
		return err
	}
	// host.json comes last: a host with it has everything else
	return files.WriteHost(host)
}

// createKeys writes this host's keys to disk and returns the cluster's keys
// and the host for the state. It does not wrap errors, because the
// functions it calls already say what failed.
func createKeys(files hostfiles.Files, clusterName string, host hostfiles.Host) (state.ClusterKeys, state.Host, error) {
	var keys state.ClusterKeys
	now := time.Now()
	ca, err := pki.NewCA(clusterName, now)
	if err != nil {
		return keys, state.Host{}, err
	}
	dataKey, err := datakey.New()
	if err != nil {
		return keys, state.Host{}, err
	}
	sealingKey, err := age.GenerateX25519Identity()
	if err != nil {
		return keys, state.Host{}, fmt.Errorf("make the cluster's sealing key: %w", err)
	}
	hostKey, err := pki.NewKey()
	if err != nil {
		return keys, state.Host{}, err
	}
	certificatePEM, err := ca.IssueHost(hostKey.Public().(ed25519.PublicKey), int(host.ID), host.Roles,
		[]netip.Addr{host.Underlay, host.NodeAddress}, now)
	if err != nil {
		return keys, state.Host{}, err
	}
	certificate, err := pki.ParseCertificate(certificatePEM)
	if err != nil {
		return keys, state.Host{}, err
	}
	wireGuardKey, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return keys, state.Host{}, fmt.Errorf("make the WireGuard key: %w", err)
	}

	hostKeyPEM, err := pki.EncodeKey(hostKey)
	if err != nil {
		return keys, state.Host{}, err
	}
	caKeyPEM, err := ca.KeyPEM()
	if err != nil {
		return keys, state.Host{}, err
	}
	if err := files.WriteKeys(ca.CertificatePEM(), certificatePEM, hostKeyPEM, wireGuardKey, dataKey); err != nil {
		return keys, state.Host{}, err
	}

	keys.CACertificate = ca.CertificatePEM()
	if keys.CAKey, err = dataKey.Encrypt(caKeyPEM); err != nil {
		return keys, state.Host{}, err
	}
	if keys.SealingKey, err = dataKey.Encrypt([]byte(sealingKey.String())); err != nil {
		return keys, state.Host{}, err
	}
	entry := state.Host{
		ID: host.ID, Name: host.Name, Roles: host.Roles, ServerID: host.ServerID, Underlay: host.Underlay,
		PublicAddress:       host.PublicAddress,
		WGPublicKey:         base64.StdEncoding.EncodeToString(wireGuardKey.PublicKey().Bytes()),
		CertificateSerial:   pki.Serial(certificate),
		CertificateNotAfter: certificate.NotAfter,
	}
	return keys, entry, nil
}

func bootstrapRaft(ctx context.Context, files hostfiles.Files, host hostfiles.Host, keys state.ClusterKeys,
	entry state.Host, clusterName string, logger *slog.Logger,
) error {
	credentials, err := files.ReadCredentials(host.Roles)
	if err != nil {
		return err
	}
	node, err := raftnode.Open(raftnode.Config{
		Dir: files.RaftDir(), ID: host.ID, Bind: host.RaftAddress(), Advertise: host.RaftAddress(),
		Certificate: credentials.Certificate, CA: credentials.CA, Logger: logger,
	})
	if err != nil {
		return err
	}
	defer func() { _ = node.Close() }() // the bootstrap is committed or failed by now
	if err := node.Bootstrap(); err != nil {
		return err
	}
	if err := waitForLead(ctx, node); err != nil {
		return err
	}
	cmd, err := state.NewCommand(state.KindBootstrap, time.Now(), state.HostActor(host.ID),
		state.Bootstrap{Keys: keys, ClusterName: clusterName, Host: entry})
	if err != nil {
		return err
	}
	if _, err := node.Propose(ctx, cmd); err != nil {
		return fmt.Errorf("bootstrap the cluster state: %w", err)
	}
	return nil
}

func waitForLead(ctx context.Context, node *raftnode.Node) error {
	deadline := time.Now().Add(leaderTimeout)
	for !node.IsLeader() {
		if time.Now().After(deadline) {
			return errors.New("the new Raft group elected no leader")
		}
		select {
		case <-time.After(50 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
