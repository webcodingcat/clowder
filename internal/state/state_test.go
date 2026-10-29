package state

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/webcodingcat/clowder/internal/clusterdir"
)

var testTime = time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)

// cluster is a state under test with its next Raft index.
type cluster struct {
	t     *testing.T
	state *State
	index uint64
}

// newCluster returns a bootstrapped cluster with host 0.
func newCluster(t *testing.T) *cluster {
	t.Helper()
	c := &cluster{t: t, state: &State{}}
	c.mustApply(KindBootstrap, LeaderActor, Bootstrap{
		Keys:    ClusterKeys{CACertificate: []byte("ca"), CAKey: Sealed("key"), AgeIdentity: Sealed("age")},
		JoinKey: Sealed("join"),
		Host:    testHost(0),
	})
	return c
}

func testHost(id HostID) Host {
	return Host{
		ID:                id,
		Name:              "node" + string(rune('0'+id)),
		Underlay:          netip.AddrFrom4([4]byte{10, 0, 0, byte(id + 2)}),
		WGPublicKey:       "wg-key",
		CertificateSerial: "serial-" + string(rune('0'+id)),
	}
}

// apply applies a command and returns Apply's error.
func (c *cluster) apply(kind, actor string, payload any) error {
	c.t.Helper()
	cmd, err := NewCommand(kind, testTime, actor, payload)
	if err != nil {
		c.t.Fatal(err)
	}
	c.index++
	return c.state.Apply(c.index, cmd)
}

func (c *cluster) mustApply(kind, actor string, payload any) {
	c.t.Helper()
	if err := c.apply(kind, actor, payload); err != nil {
		c.t.Fatal(err)
	}
}

// encoded returns the state as JSON without the index, to compare states.
func (c *cluster) encoded() string {
	c.t.Helper()
	copied := *c.state
	copied.Index = 0
	encoded, err := json.Marshal(copied)
	if err != nil {
		c.t.Fatal(err)
	}
	return string(encoded)
}

// refuse applies a command that must fail and must leave the state as it
// was.
func (c *cluster) refuse(kind, actor string, payload any, wantError string) {
	c.t.Helper()
	before := c.encoded()
	err := c.apply(kind, actor, payload)
	if err == nil || !strings.Contains(err.Error(), wantError) {
		c.t.Errorf("%s: want an error with %q, got %v", kind, wantError, err)
	}
	if after := c.encoded(); after != before {
		c.t.Errorf("%s: the refused command changed the state:\nbefore %s\nafter  %s", kind, before, after)
	}
}

func TestBootstrapMakesHostZeroTheActiveVoter(t *testing.T) {
	c := newCluster(t)
	host := c.state.Hosts[0]
	if host.Phase != Active || !host.Voter {
		t.Errorf("host 0: %+v", host)
	}
	if len(c.state.JoinKeys) != 1 || c.state.Keys == nil {
		t.Errorf("keys: %+v %v", c.state.Keys, c.state.JoinKeys)
	}
	c.refuse(KindBootstrap, LeaderActor, Bootstrap{}, "bootstrapped already")
}

func TestNothingButTheBootstrapRunsOnAnEmptyState(t *testing.T) {
	state := &State{}
	cmd, err := NewCommand(KindAddUser, testTime, LeaderActor, AddUser{Name: "alice", Serial: "1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Apply(1, cmd); !errors.Is(err, ErrNotBootstrapped) {
		t.Errorf("want ErrNotBootstrapped, got %v", err)
	}
	if state.Index != 1 {
		t.Errorf("the index must follow the log even for refused commands: %d", state.Index)
	}
}

func TestCommandsOfUnknownVersionsOrKindsAreRefused(t *testing.T) {
	c := newCluster(t)
	for _, test := range []struct {
		version int
		kind    string
		want    string
	}{
		{CommandVersion + 1, KindAddUser, "not readable"},
		{CommandVersion - 2, KindAddUser, "not readable"},
		{0, KindAddUser, "not readable"}, // a command without a version field
		{CommandVersion, "make-coffee", "unknown command"},
	} {
		cmd := Command{Version: test.version, Kind: test.kind, At: testTime, Actor: LeaderActor, Payload: []byte("{}")}
		if err := c.state.Apply(99, cmd); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%d %s: want %q, got %v", test.version, test.kind, test.want, err)
		}
	}
}

func TestAddHostChecksTheID(t *testing.T) {
	c := newCluster(t)
	c.mustApply(KindAddHost, LeaderActor, AddHost{Host: testHost(1)})
	if host := c.state.Hosts[1]; host.Phase != Joining || host.Voter {
		t.Errorf("a new host joins as non-voter: %+v", host)
	}
	c.refuse(KindAddHost, LeaderActor, AddHost{Host: testHost(1)}, "taken")
	c.refuse(KindAddHost, LeaderActor, AddHost{Host: testHost(MaxHosts)}, "not from 0")
	sameName := testHost(2)
	sameName.Name = "node0"
	c.refuse(KindAddHost, LeaderActor, AddHost{Host: sameName}, "member already")
	noKey := testHost(3)
	noKey.WGPublicKey = ""
	c.refuse(KindAddHost, LeaderActor, AddHost{Host: noKey}, "WireGuard")
}

func TestHostPhasesMoveOnlyAsAllowed(t *testing.T) {
	tests := []struct {
		name   string
		from   HostPhase
		to     HostPhase
		actor  string
		reason string // empty: allowed
	}{
		{"leader drains", Active, Draining, UserActor("alice"), ""},
		{"leader undrains", Draining, Active, LeaderActor, ""},
		{"leader declares failed", Active, Failed, LeaderActor, ""},
		{"host is back before fencing", Failed, Active, HostActor(1), ""},
		{"leader fences", Failed, Fenced, LeaderActor, ""},
		{"host recovers", Fenced, Recovering, HostActor(1), ""},
		{"host is up to date", Recovering, Active, HostActor(1), ""},
		{"flapping brake", Fenced, Disabled, LeaderActor, ""},
		{"leader removes from any phase", Recovering, Removed, LeaderActor, ""},
		{"host cannot declare itself failed", Active, Failed, HostActor(1), "only the leader"},
		{"leader cannot report recovery", Fenced, Recovering, LeaderActor, "only host"},
		{"another host cannot report", Failed, Active, HostActor(2), "only host"},
		{"another host cannot declare failed", Active, Failed, HostActor(2), "only the leader"},
		{"another host cannot remove", Active, Removed, HostActor(2), "only the leader"},
		{"no shortcut", Fenced, Active, HostActor(1), "cannot go from"},
		{"removed stays removed", Removed, Active, LeaderActor, "removed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := newCluster(t)
			c.mustApply(KindAddHost, LeaderActor, AddHost{Host: testHost(1)})
			c.state.Hosts[1].Phase = test.from
			payload := SetHostPhase{Host: 1, Phase: test.to}
			if test.reason != "" {
				c.refuse(KindSetHostPhase, test.actor, payload, test.reason)
				return
			}
			c.mustApply(KindSetHostPhase, test.actor, payload)
			if c.state.Hosts[1].Phase != test.to {
				t.Errorf("phase %s", c.state.Hosts[1].Phase)
			}
		})
	}
}

func TestEveryPhaseHasAWayOut(t *testing.T) {
	for _, phase := range []HostPhase{Joining, Passive, Active, Draining, Failed, Fenced, Recovering, Disabled} {
		if len(phaseChanges[phase]) == 0 {
			t.Errorf("%s has no allowed change", phase)
		}
	}
}

func TestOnlyAHostReportsAboutItself(t *testing.T) {
	c := newCluster(t)
	c.mustApply(KindAddHost, LeaderActor, AddHost{Host: testHost(1)})
	pools := map[string]Pool{"vg-fast/thin": {Name: "vg-fast/thin", SizeBytes: 100 << 30, UsedBytes: 1 << 30}}
	c.mustApply(KindReportHost, HostActor(0), ReportHost{Host: 0, Version: "1.3.0", Pools: pools})
	c.mustApply(KindReportHost, HostActor(1), ReportHost{Host: 1, Version: "1.2.9"})
	c.refuse(KindReportHost, HostActor(1), ReportHost{Host: 0, Version: "9.9.9"}, "only host")
	c.refuse(KindReportHost, HostActor(1), ReportHost{
		Host: 1, Version: "1.2.9",
		Pools: map[string]Pool{"vg/thin": {SizeBytes: 0}},
	}, "pool vg/thin")
	if got := c.state.MinVersion(); got != "1.2.9" {
		t.Errorf("MinVersion %s", got)
	}
	if c.state.Hosts[0].Pools["vg-fast/thin"].SizeBytes != 100<<30 {
		t.Errorf("pools %v", c.state.Hosts[0].Pools)
	}
}

func TestOnlyOneFirstUser(t *testing.T) {
	c := newCluster(t)
	c.mustApply(KindAddUser, LeaderActor, AddUser{Name: "alice", Serial: "a1", First: true})
	c.refuse(KindAddUser, LeaderActor, AddUser{Name: "bob", Serial: "b1", First: true}, "first user already")
}

func TestRemovingAUserRevokesTheirCertificates(t *testing.T) {
	c := newCluster(t)
	c.mustApply(KindAddUser, LeaderActor, AddUser{Name: "alice", Serial: "a1"})
	c.mustApply(KindAddUser, LeaderActor, AddUser{Name: "alice", Serial: "a2"})
	c.mustApply(KindAddUser, LeaderActor, AddUser{Name: "bob", Serial: "b1"})
	c.refuse(KindAddUser, LeaderActor, AddUser{Name: "carol", Serial: "a1"}, "used already")

	c.mustApply(KindRevoke, UserActor("alice"), Revoke{Serial: "a1"})
	if serials := c.state.Users["alice"].Serials; len(serials) != 1 || serials[0] != "a2" {
		t.Errorf("alice keeps only a2: %v", serials)
	}
	c.refuse(KindAddUser, LeaderActor, AddUser{Name: "alice", Serial: "a1"}, "used already")

	c.mustApply(KindRemoveUser, UserActor("bob"), RemoveUser{Name: "alice"})
	if _, ok := c.state.Users["alice"]; ok {
		t.Error("alice is still a user")
	}
	for _, serial := range []string{"a1", "a2"} {
		if at, ok := c.state.Revoked[serial]; !ok || !at.Equal(testTime) {
			t.Errorf("%s not revoked at the command's time: %v", serial, at)
		}
	}
}

func testConfig() clusterdir.Config {
	return clusterdir.Config{
		Name: "prod", Seeds: []string{"10.0.0.2"}, Hoster: "hetzner",
		HosterToken:    clusterdir.SecretRef{Secret: "hcloud-token"},
		Ingress:        clusterdir.Ingress{FloatingIP: "203.0.113.11"},
		StorageClasses: map[string]clusterdir.StorageClass{"fast": {Pool: "vg-fast/thin"}},
	}
}

func TestApplyConfigCountsRevisionsAndReplacesTheSecrets(t *testing.T) {
	c := newCluster(t)
	secrets := map[string]Sealed{"hcloud-token": Sealed("sealed")}
	c.mustApply(KindApplyConfig, UserActor("alice"), ApplyConfig{
		Hash: "h1", Spec: testConfig(), Secrets: secrets, JoinKeys: []Sealed{Sealed("k1"), Sealed("k2")},
	})
	c.mustApply(KindApplyConfig, UserActor("alice"), ApplyConfig{
		Hash: "h2", Spec: testConfig(), Secrets: secrets, JoinKeys: []Sealed{Sealed("k2")},
	})
	if c.state.Config.Revision != 2 || c.state.Config.Hash != "h2" || len(c.state.JoinKeys) != 1 {
		t.Errorf("config %+v, join keys %v", c.state.Config, c.state.JoinKeys)
	}
	c.refuse(KindApplyConfig, UserActor("alice"), ApplyConfig{Spec: testConfig()}, "hcloud-token")
	c.refuse(KindApplyConfig, UserActor("alice"), ApplyConfig{Secrets: secrets}, "name is required")
}

func TestStacksBelongToTheirOwner(t *testing.T) {
	c := newCluster(t)
	spec := "services: {}"
	c.mustApply(KindPutStack, UserActor("alice"), PutStack{Name: "shop", Owner: OwnerCLI, Source: spec})
	c.mustApply(KindPutStack, UserActor("alice"), PutStack{Name: "shop", Owner: OwnerCLI, Source: spec, Priority: 700})
	stack := c.state.Stacks["shop"]
	if stack.Generation != 2 || stack.Status != StackPending || stack.Priority != 700 {
		t.Errorf("stack %+v", stack)
	}
	c.refuse(KindPutStack, LeaderActor, PutStack{Name: "shop", Owner: GitOpsOwner("main"), Source: spec}, "belongs to cli")

	host := HostID(0)
	c.refuse(KindSetStackStatus, HostActor(0), SetStackStatus{Name: "shop", Status: StackRunning, Host: &host},
		"only its host")
	c.mustApply(KindSetStackStatus, LeaderActor, SetStackStatus{Name: "shop", Status: StackStarting, Host: &host})
	if !stack.LastMove.Equal(testTime) {
		t.Errorf("placing a stack is a move: %v", stack.LastMove)
	}
	c.mustApply(KindSetStackStatus, HostActor(0), SetStackStatus{Name: "shop", Status: StackRunning, Host: &host})
	c.refuse(KindSetStackStatus, HostActor(1), SetStackStatus{Name: "shop", Status: StackStopped, Host: &host},
		"only its host")
	c.refuse(KindSetStackStatus, HostActor(0), SetStackStatus{Name: "shop", Status: StackPending}, "only where it is")
	gone := HostID(7)
	c.refuse(KindSetStackStatus, LeaderActor, SetStackStatus{Name: "shop", Status: StackRunning, Host: &gone}, "not a member")
	c.refuse(KindSetStackStatus, LeaderActor, SetStackStatus{Name: "shop", Status: "dancing"}, "unknown status")
}

func TestVolumesNeedTheirStackAndOwnMinors(t *testing.T) {
	c := newCluster(t)
	c.mustApply(KindPutStack, UserActor("alice"), PutStack{Name: "shop", Owner: OwnerCLI, Source: "services: {}"})
	minor := 5
	volume := Volume{
		ID: "shop/pgdata", Generation: 1, Class: "fast", SizeBytes: 1 << 30, Replicas: 1, Minor: &minor,
		Placements: []Placement{{Host: 0, Class: "fast"}}, Status: VolumeOK,
	}
	c.mustApply(KindPutVolume, LeaderActor, PutVolume{Volume: volume})

	other := volume
	other.ID = "shop/cache"
	c.refuse(KindPutVolume, LeaderActor, PutVolume{Volume: other}, "minor 5 belongs to shop/pgdata")
	orphan := volume
	orphan.ID = "blog/data"
	c.refuse(KindPutVolume, LeaderActor, PutVolume{Volume: orphan}, "no stack blog")
	older := volume
	older.Generation = 0
	c.refuse(KindPutVolume, LeaderActor, PutVolume{Volume: older}, "older")
	tooHigh := 1000
	other.Minor = &tooHigh
	c.refuse(KindPutVolume, LeaderActor, PutVolume{Volume: other}, "not from 0 to 999")
	twice := volume
	twice.Replicas = 2
	twice.Placements = []Placement{{Host: 0, Class: "fast"}, {Host: 0, Class: "fast"}}
	c.refuse(KindPutVolume, LeaderActor, PutVolume{Volume: twice}, "two copies on host 0")
	tooMany := volume
	tooMany.Replicas = 4
	c.refuse(KindPutVolume, LeaderActor, PutVolume{Volume: tooMany}, "not 0 to 3")
	noSize := volume
	noSize.SizeBytes = 0
	c.refuse(KindPutVolume, LeaderActor, PutVolume{Volume: noSize}, "needs a class and a size")
	plain := volume
	plain.Replicas = 0
	c.refuse(KindPutVolume, LeaderActor, PutVolume{Volume: plain}, "a DRBD minor exactly when")
	strange := volume
	strange.Status = "melting"
	c.refuse(KindPutVolume, LeaderActor, PutVolume{Volume: strange}, "unknown status")

	c.refuse(KindDeleteStack, UserActor("alice"), DeleteStack{Name: "shop"}, "still has volume shop/pgdata")
	c.mustApply(KindDeleteStack, UserActor("alice"), DeleteStack{Name: "shop", WithVolumes: true})
	if len(c.state.Volumes) != 0 || len(c.state.Stacks) != 0 {
		t.Errorf("left behind: %v, %v", c.state.Volumes, c.state.Stacks)
	}
}

func TestOperationsHoldTheirLocks(t *testing.T) {
	c := newCluster(t)
	c.mustApply(KindPutStack, UserActor("alice"), PutStack{Name: "shop", Owner: OwnerCLI, Source: "services: {}"})
	move := Operation{ID: "01A", Kind: "move-stack", Locks: []string{"stack/shop", "volume/shop/pgdata"}}
	c.mustApply(KindStartOperation, LeaderActor, StartOperation{Operation: move})

	c.refuse(KindPutStack, UserActor("alice"), PutStack{Name: "shop", Owner: OwnerCLI, Source: "services: {}"},
		"locked by operation 01A")
	// the operation itself may change what it holds
	c.mustApply(KindPutStack, LeaderActor, PutStack{Name: "shop", Owner: OwnerCLI, Source: "services: {}", Operation: "01A"})
	backup := Operation{ID: "01B", Kind: "backup", Locks: []string{"volume/shop/pgdata"}}
	before := c.encoded()
	err := c.apply(KindStartOperation, LeaderActor, StartOperation{Operation: backup})
	var locked *LockedError
	if !errors.As(err, &locked) || locked.Operation != "01A" {
		t.Errorf("want a LockedError naming 01A, got %v", err)
	}
	if c.encoded() != before {
		t.Error("a refused operation changed the state")
	}

	owner := HostID(0)
	c.mustApply(KindAdvanceOperation, LeaderActor, AdvanceOperation{ID: "01A", Step: 3, Owner: &owner, RollingBack: true})
	if op := c.state.Operations["01A"]; op.Step != 3 || op.State != OpRollingBack {
		t.Errorf("operation %+v", op)
	}
	c.mustApply(KindFinishOperation, LeaderActor, FinishOperation{ID: "01A", Failed: true, Error: "resync stalled"})
	if len(c.state.Locks) != 0 || c.state.Operations["01A"].State != OpFailed {
		t.Errorf("locks %v, operation %+v", c.state.Locks, c.state.Operations["01A"])
	}
	c.refuse(KindFinishOperation, LeaderActor, FinishOperation{ID: "01A"}, "finished")
	c.mustApply(KindStartOperation, LeaderActor, StartOperation{Operation: backup})
}

func TestLocksNameAStackOrAVolume(t *testing.T) {
	c := newCluster(t)
	for _, lock := range []string{"x", "stack/", "volume/shop", "volume/Shop/data"} {
		c.refuse(KindStartOperation, LeaderActor, StartOperation{Operation: Operation{
			ID: "01A", Kind: "backup", Locks: []string{lock},
		}}, "neither stack")
	}
}

func TestFailoverTakesOverLocks(t *testing.T) {
	c := newCluster(t)
	c.mustApply(KindStartOperation, LeaderActor, StartOperation{Operation: Operation{
		ID: "01A", Kind: "backup", Locks: []string{"volume/shop/pgdata", "volume/shop/files"},
	}})
	c.mustApply(KindStartOperation, LeaderActor, StartOperation{TakeOver: true, Operation: Operation{
		ID: "01B", Kind: "failover", Locks: []string{"volume/shop/pgdata"},
	}})
	if op := c.state.Operations["01A"]; op.State != OpFailed || !strings.Contains(op.Error, "01B") {
		t.Errorf("the taken-over operation: %+v", op)
	}
	if _, held := c.state.Locks["volume/shop/files"]; held {
		t.Error("the failed operation still holds its other lock")
	}
	if c.state.Locks["volume/shop/pgdata"] != "01B" {
		t.Errorf("locks %v", c.state.Locks)
	}
}

func TestOnlyTheLastFinishedOperationsAreKept(t *testing.T) {
	c := newCluster(t)
	for number := range keptFinished + 5 {
		id := string(rune('A'+number/26)) + string(rune('A'+number%26))
		c.mustApply(KindStartOperation, LeaderActor, StartOperation{Operation: Operation{
			ID: id, Kind: "backup", Locks: []string{"volume/shop/pgdata"},
		}})
		c.mustApply(KindFinishOperation, LeaderActor, FinishOperation{ID: id})
	}
	if len(c.state.Operations) != keptFinished {
		t.Errorf("%d operations kept", len(c.state.Operations))
	}
	if _, ok := c.state.Operations["AA"]; ok {
		t.Error("the oldest operation is still there")
	}
}

func TestTheAuditKeepsTheLastEntries(t *testing.T) {
	c := newCluster(t)
	for range auditSize + 10 {
		c.mustApply(KindReportHost, HostActor(0), ReportHost{Host: 0, Version: "1.0.0"})
	}
	audit := c.state.Audit
	if len(audit) != auditSize || audit[len(audit)-1].Index != c.index || audit[0].Index != c.index-auditSize+1 {
		t.Errorf("%d entries from %d to %d", len(audit), audit[0].Index, audit[len(audit)-1].Index)
	}
	if last := audit[len(audit)-1]; last.Actor != HostActor(0) || last.Kind != KindReportHost {
		t.Errorf("last entry %+v", last)
	}
}

func TestSnapshotsGiveTheSameState(t *testing.T) {
	c := newCluster(t)
	c.mustApply(KindAddHost, LeaderActor, AddHost{Host: testHost(1)})
	c.mustApply(KindAddUser, LeaderActor, AddUser{Name: "alice", Serial: "a1"})
	c.mustApply(KindPutStack, UserActor("alice"), PutStack{Name: "shop", Owner: OwnerCLI, Source: "services: {a: {image: x}}"})

	var buffer bytes.Buffer
	if err := c.state.WriteSnapshot(&buffer); err != nil {
		t.Fatal(err)
	}
	restored, err := ReadSnapshot(&buffer)
	if err != nil {
		t.Fatal(err)
	}
	copied := &cluster{t: t, state: restored, index: c.index}
	if copied.encoded() != c.encoded() || restored.Index != c.state.Index {
		t.Errorf("restored state differs:\n%s\n%s", copied.encoded(), c.encoded())
	}
	// the restored state goes on taking commands
	copied.mustApply(KindAddUser, LeaderActor, AddUser{Name: "bob", Serial: "b1"})
}

func TestSnapshotsOfUnknownVersionsAreRefused(t *testing.T) {
	var buffer bytes.Buffer
	if err := (&State{}).WriteSnapshot(&buffer); err != nil {
		t.Fatal(err)
	}
	future := bytes.Replace(gunzip(t, buffer.Bytes()), []byte(`"version":1`), []byte(`"version":2`), 1)
	if _, err := ReadSnapshot(bytes.NewReader(gzipped(t, future))); err == nil || !strings.Contains(err.Error(), "not readable") {
		t.Errorf("want a version error, got %v", err)
	}
}

func TestTwoAgentsApplyingTheSameLogAgree(t *testing.T) {
	first, second := newCluster(t), newCluster(t)
	for _, c := range []*cluster{first, second} {
		c.mustApply(KindAddHost, LeaderActor, AddHost{Host: testHost(1)})
		c.mustApply(KindAddHost, LeaderActor, AddHost{Host: testHost(2)})
		c.mustApply(KindAddUser, LeaderActor, AddUser{Name: "alice", Serial: "a1"})
		c.mustApply(KindRemoveUser, LeaderActor, RemoveUser{Name: "alice"})
		_ = c.apply(KindRevoke, LeaderActor, Revoke{Serial: "a1"}) // refused on both
	}
	if first.encoded() != second.encoded() {
		t.Errorf("the states differ:\n%s\n%s", first.encoded(), second.encoded())
	}
}

func gunzip(t *testing.T, compressed []byte) []byte {
	t.Helper()
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return plain
}

func gzipped(t *testing.T, plain []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	if _, err := writer.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestRemovingAHostRevokesItsCertificate(t *testing.T) {
	c := newCluster(t)
	c.mustApply(KindAddHost, LeaderActor, AddHost{Host: testHost(1)})
	c.mustApply(KindSetHostPhase, UserActor("alice"), SetHostPhase{Host: 1, Phase: Removed})
	if at, revoked := c.state.Revoked[testHost(1).CertificateSerial]; !revoked || !at.Equal(testTime) {
		t.Errorf("the removed host's certificate is not revoked: %v", c.state.Revoked)
	}
}
