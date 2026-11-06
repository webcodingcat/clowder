package raftnode

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/webcodingcat/clowder/internal/pki"
	"github.com/webcodingcat/clowder/internal/resource"
	"github.com/webcodingcat/clowder/internal/state"
)

// testCluster is a CA and the hosts' Raft members on the loopback address.
type testCluster struct {
	t  *testing.T
	ca *pki.CA
}

func newTestCluster(t *testing.T) *testCluster {
	t.Helper()
	ca, err := pki.NewCA("test", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return &testCluster{t: t, ca: ca}
}

// certificate issues a host or user certificate for 127.0.0.1.
func (c *testCluster) certificate(issue func(public ed25519.PublicKey) ([]byte, error)) tls.Certificate {
	c.t.Helper()
	key, err := pki.NewKey()
	if err != nil {
		c.t.Fatal(err)
	}
	certificatePEM, err := issue(key.Public().(ed25519.PublicKey))
	if err != nil {
		c.t.Fatal(err)
	}
	leaf, err := pki.ParseCertificate(certificatePEM)
	if err != nil {
		c.t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{leaf.Raw}, PrivateKey: key, Leaf: leaf}
}

func (c *testCluster) hostCertificate(id state.HostID) tls.Certificate {
	return c.certificate(func(public ed25519.PublicKey) ([]byte, error) {
		return c.ca.IssueHost(public, int(id), []netip.Addr{netip.MustParseAddr("127.0.0.1")}, time.Now())
	})
}

// freeAddress returns a loopback address with a port nobody uses right now.
func freeAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

// open starts a member for host id in dir at address.
func (c *testCluster) open(id state.HostID, dir, address string) *Node {
	c.t.Helper()
	node, err := Open(Config{
		Dir: dir, ID: id, Bind: address, Advertise: address,
		Certificate: c.hostCertificate(id), CA: c.ca.Certificate,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		c.t.Fatal(err)
	}
	return node
}

func waitForLeader(t *testing.T, node *Node) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !node.IsLeader() {
		if time.Now().After(deadline) {
			t.Fatal("no leader after 15 s")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func command(t *testing.T, kind string, payload any) state.Command {
	t.Helper()
	cmd, err := state.NewCommand(kind, time.Now(), state.LeaderActor, payload)
	if err != nil {
		t.Fatal(err)
	}
	return cmd
}

func bootstrapCommand(t *testing.T) state.Command {
	return command(t, state.KindBootstrap, state.Bootstrap{
		Keys:        state.ClusterKeys{CACertificate: []byte("ca"), CAKey: state.Encrypted("key"), SealingKey: state.Encrypted("age")},
		ClusterName: "prod",
		Host: state.Host{
			ID: 0, Name: "node0", ServerID: 40, Underlay: netip.MustParseAddr("10.0.0.2"),
			PublicAddress: netip.MustParseAddr("198.51.100.2"), WGPublicKey: "wg", CertificateSerial: "s0",
			CertificateNotAfter: time.Now().AddDate(1, 0, 0),
		},
	})
}

// traceCutoff is the CLI's cutoff that setCutoff leaves in the state.
var traceCutoff = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// setCutoff is a command after the bootstrap that leaves a trace in the
// state: the CLI's cutoff. The state refuses it a second time, as a cutoff
// never moves back.
func setCutoff(t *testing.T) state.Command {
	t.Helper()
	cmd, err := state.NewCommand(state.KindSetCLICutoff, time.Now(), state.HostActor(0),
		state.SetCLICutoff{Cutoff: traceCutoff})
	if err != nil {
		t.Fatal(err)
	}
	return cmd
}

// cutoffSet tells whether setCutoff left its trace in the node's state.
func cutoffSet(node *Node) bool {
	var cutoff time.Time
	node.Read(func(current *state.State) { cutoff = current.CLICutoff })
	return cutoff.Equal(traceCutoff)
}

func TestASingleHostKeepsItsStateAcrossARestart(t *testing.T) {
	cluster := newTestCluster(t)
	dir, address := t.TempDir(), freeAddress(t)
	node := cluster.open(0, dir, address)
	if err := node.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	waitForLeader(t, node)
	ctx := context.Background()
	if _, err := node.Propose(ctx, bootstrapCommand(t)); err != nil {
		t.Fatal(err)
	}
	// the snapshot plus the entry after it must give the same state
	if err := node.raft.Snapshot().Error(); err != nil {
		t.Fatal(err)
	}
	index, err := node.Propose(ctx, setCutoff(t))
	if err != nil {
		t.Fatal(err)
	}
	// a refused command comes back as the state's error
	_, err = node.Propose(ctx, setCutoff(t))
	var refused *state.Refused
	if !errors.As(err, &refused) {
		t.Errorf("want a refusal for a cutoff that is not later, got %v", err)
	}
	if err := node.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := cluster.open(0, dir, address)
	defer func() { _ = reopened.Close() }()
	if err := reopened.Bootstrap(); err != nil {
		t.Fatalf("bootstrapping a member with a log must do nothing: %v", err)
	}
	waitForLeader(t, reopened)
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := reopened.WaitIndex(waitCtx, index); err != nil {
		t.Fatal(err)
	}
	if !cutoffSet(reopened) {
		t.Error("the cutoff is gone after the restart")
	}
}

func TestTheLeaderHoldsBackAKindAnOlderAgentCannotRead(t *testing.T) {
	cluster := newTestCluster(t)
	node := cluster.open(0, t.TempDir(), freeAddress(t))
	defer func() { _ = node.Close() }()
	if err := node.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	waitForLeader(t, node)
	ctx := context.Background()
	if _, err := node.Propose(ctx, bootstrapCommand(t)); err != nil {
		t.Fatal(err)
	}
	report, err := state.NewCommand(state.KindReportHost, time.Now(), state.HostActor(0),
		state.ReportHost{Host: 0, Version: "0.1.0"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := node.Propose(ctx, report); err != nil {
		t.Fatal(err)
	}
	initialized := command(t, state.KindSetVolumeInitialized, state.SetVolumeInitialized{Volume: "shop/data"})
	_, err = node.Propose(ctx, initialized)
	var refused *state.Refused
	if !errors.Is(err, state.ErrNotReadByAll) || errors.As(err, &refused) {
		t.Errorf("a kind of 0.2.0 with an agent of 0.1.0: %v", err)
	}
}

func TestTheWatchSeesWhatTheLogChangedAfterItsSnapshot(t *testing.T) {
	cluster := newTestCluster(t)
	dir, address := t.TempDir(), freeAddress(t)
	node := cluster.open(0, dir, address)
	if err := node.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	waitForLeader(t, node)
	ctx := context.Background()
	namespace := func(on *Node, name string) uint64 {
		t.Helper()
		index, err := on.Propose(ctx, command(t, state.KindChangeResources, state.ChangeResources{
			Changes: []state.ResourceChange{{Put: &state.PutResource{
				Kind: resource.KindNamespace, Name: name, Owner: state.OwnerCLI,
			}}},
		}))
		if err != nil {
			t.Fatal(err)
		}
		return index
	}
	if _, err := node.Propose(ctx, bootstrapCommand(t)); err != nil {
		t.Fatal(err)
	}
	shop := namespace(node, "shop")
	changes, _, err := node.Changes(0)
	if err != nil || len(changes) == 0 || changes[len(changes)-1].Name != "shop" {
		t.Fatalf("changes since 0: %v, %v", changes, err)
	}
	if err := node.raft.Snapshot().Error(); err != nil {
		t.Fatal(err)
	}
	blog := namespace(node, "blog")
	if err := node.Close(); err != nil {
		t.Fatal(err)
	}

	// restarted from the snapshot, the ring holds what the log replayed
	// after it, and says so for what came before
	reopened := cluster.open(0, dir, address)
	defer func() { _ = reopened.Close() }()
	waitForLeader(t, reopened)
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := reopened.WaitIndex(waitCtx, blog); err != nil {
		t.Fatal(err)
	}
	if _, _, err := reopened.Changes(shop - 1); !errors.Is(err, state.ErrChangesGone) {
		t.Errorf("since before the snapshot: %v", err)
	}
	changes, index, err := reopened.Changes(shop)
	if err != nil || len(changes) != 1 || changes[0].Name != "blog" || index < blog {
		t.Errorf("since the snapshot: %v, index %d, %v", changes, index, err)
	}
}

func TestThreeHostsReplicateOverMutualTLS(t *testing.T) {
	cluster := newTestCluster(t)
	var nodes []*Node
	var addresses []string
	for id := range state.HostID(3) {
		address := freeAddress(t)
		node := cluster.open(id, t.TempDir(), address)
		defer func() { _ = node.Close() }()
		nodes, addresses = append(nodes, node), append(addresses, address)
	}
	if err := nodes[0].Bootstrap(); err != nil {
		t.Fatal(err)
	}
	waitForLeader(t, nodes[0])
	for id := 1; id < 3; id++ {
		if err := nodes[0].AddVoter(state.HostID(id), addresses[id]); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	if _, err := nodes[0].Propose(ctx, bootstrapCommand(t)); err != nil {
		t.Fatal(err)
	}
	index, err := nodes[0].Propose(ctx, setCutoff(t))
	if err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for id, node := range nodes {
		if err := node.WaitIndex(waitCtx, index); err != nil {
			t.Fatalf("host %d: %v", id, err)
		}
		if !cutoffSet(node) {
			t.Errorf("host %d lacks the cutoff", id)
		}
	}
	if id, ok := nodes[2].Leader(); !ok || id != 0 {
		t.Errorf("host 2 sees leader %d (%v)", id, ok)
	}
	if _, err := nodes[1].Propose(ctx, setCutoff(t)); err == nil {
		t.Error("a follower committed a command")
	}
	if err := nodes[0].VerifyLeader(waitCtx); err != nil {
		t.Errorf("the leader: %v", err)
	}
	if err := nodes[1].VerifyLeader(waitCtx); err == nil {
		t.Error("a follower verified a lead")
	}
	// the last entry is Raft's own, which the state never sees: a follower
	// still catches up with the leader's commit index
	if err := nodes[0].DemoteVoter(2); err != nil {
		t.Fatal(err)
	}
	commitIndex := nodes[0].CommitIndex()
	if commitIndex <= index {
		t.Fatalf("commit index %d, the last command was %d", commitIndex, index)
	}
	if err := nodes[1].WaitCommitted(waitCtx, commitIndex); err != nil {
		t.Errorf("host 1: %v", err)
	}
}

func TestOnlyHostsOfTheClusterGetIn(t *testing.T) {
	cluster := newTestCluster(t)
	address := freeAddress(t)
	node := cluster.open(0, t.TempDir(), address)
	defer func() { _ = node.Close() }()

	cli := cluster.certificate(func(public ed25519.PublicKey) ([]byte, error) {
		return cluster.ca.IssueCLI(public, "alice", time.Now())
	})
	stranger := newTestCluster(t).hostCertificate(1)
	for name, certificate := range map[string]tls.Certificate{"the CLI": cli, "another cluster's host": stranger} {
		connection, err := tls.Dial("tcp", address, &tls.Config{
			Certificates: []tls.Certificate{certificate}, InsecureSkipVerify: true, // only the server's check is tested
			MinVersion: tls.VersionTLS13,
		})
		if err == nil {
			// TLS 1.3 reports the server's refusal on the first read
			_ = connection.SetReadDeadline(time.Now().Add(5 * time.Second))
			_, err = connection.Read(make([]byte, 1))
			_ = connection.Close()
		}
		if err == nil {
			t.Errorf("%s got in", name)
		}
	}
}
