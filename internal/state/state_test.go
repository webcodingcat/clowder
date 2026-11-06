package state

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/webcodingcat/clowder/internal/buildinfo"
	"github.com/webcodingcat/clowder/internal/clusterspec"
	"github.com/webcodingcat/clowder/internal/hostrole"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/resource"
)

var testTime = time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)

// testExpiry is when the tests' certificates expire.
var testExpiry = testTime.AddDate(10, 0, 0)

// cliActor is the CLI of the tests, with the name every certificate gets
// without one.
var cliActor = CLIActor("admin")

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
		Keys:        ClusterKeys{CACertificate: []byte("ca"), CAKey: Encrypted("key"), SealingKey: Encrypted("age")},
		ClusterName: "prod",
		Host:        testHost(0),
	})
	return c
}

// addHost joins a host with a token of its own; it is joining.
func (c *cluster) addHost(host Host) {
	c.t.Helper()
	token := "token-" + strconv.Itoa(int(host.ID))
	c.mustApply(KindAddJoinToken, cliActor, joinToken(token))
	host.ServerChecked = true
	c.mustApply(KindAddHost, LeaderActor, AddHost{Token: token, Host: host})
}

// testHash is the hash of the tests' join tokens.
var testHash = strings.Repeat("ab", 32)

// joinToken is a token for an hour that allows every role.
func joinToken(id string) AddJoinToken {
	return AddJoinToken{ID: id, Hash: testHash, TTL: time.Hour, Roles: hostrole.DefaultInit}
}

// testHost is a host with every role, so that it does everything.
func testHost(id HostID) Host {
	return Host{
		ID:                  id,
		Name:                "node" + strconv.Itoa(int(id)),
		Roles:               hostrole.DefaultInit,
		ServerID:            40 + int64(id),
		Underlay:            netip.AddrFrom4([4]byte{10, 0, 0, byte(id + 2)}),
		PublicAddress:       netip.AddrFrom4([4]byte{198, 51, 100, byte(id + 2)}),
		WGPublicKey:         "wg-key",
		CertificateSerial:   "serial-" + strconv.Itoa(int(id)),
		CertificateNotAfter: testExpiry,
	}
}

// changes applies a command and returns what Apply returns. A command the
// test may see refused must leave the state as it was.
func (c *cluster) changes(kind, actor string, payload any) ([]Change, error) {
	c.t.Helper()
	before := c.handled()
	changes, err := c.applyAt(testTime, kind, actor, payload)
	if after := c.handled(); err != nil && after != before {
		c.t.Errorf("%s: the refused command changed the state:\nbefore %s\nafter  %s", kind, before, after)
	}
	return changes, err
}

// applyAt applies a command of the given time.
func (c *cluster) applyAt(at time.Time, kind, actor string, payload any) ([]Change, error) {
	c.t.Helper()
	cmd, err := NewCommand(kind, at, actor, payload)
	if err != nil {
		c.t.Fatal(err)
	}
	c.index++
	return c.state.Apply(c.index, cmd)
}

// apply applies a command and returns Apply's error.
func (c *cluster) apply(kind, actor string, payload any) error {
	c.t.Helper()
	_, err := c.changes(kind, actor, payload)
	return err
}

// mustApply applies a command that must pass. It skips the check of
// changes, which encodes the state twice: tests fill it with a thousand
// commands.
func (c *cluster) mustApply(kind, actor string, payload any) {
	c.t.Helper()
	if _, err := c.applyAt(testTime, kind, actor, payload); err != nil {
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

// handled returns what the handlers may change as JSON: the state without
// the index and the audit, which only Apply writes.
func (c *cluster) handled() string {
	c.t.Helper()
	copied := *c.state
	copied.Index, copied.Audit = 0, nil
	encoded, err := json.Marshal(copied)
	if err != nil {
		c.t.Fatal(err)
	}
	return string(encoded)
}

// refuse applies a command that must fail (and so leave the state as it
// was, which changes checks).
func (c *cluster) refuse(kind, actor string, payload any, wantError string) {
	c.t.Helper()
	err := c.apply(kind, actor, payload)
	if err == nil || !strings.Contains(err.Error(), wantError) {
		c.t.Errorf("%s: want an error with %q, got %v", kind, wantError, err)
	}
}

// putChange is how clowder apply sends one resource.
func putChange(kind, namespace, name string, spec any) ResourceChange {
	encoded, err := json.Marshal(spec)
	if err != nil {
		panic(err)
	}
	return ResourceChange{Put: &PutResource{Kind: kind, Namespace: namespace, Name: name, Owner: OwnerCLI, Spec: encoded}}
}

// deleteChange is how clowder delete sends one resource.
func deleteChange(kind, namespace, name string) ResourceChange {
	return ResourceChange{Delete: &DeleteResource{Kind: kind, Namespace: namespace, Name: name, Owner: OwnerCLI}}
}

// batch is one change-resources of the changes, in their order.
func batch(changes ...ResourceChange) ChangeResources {
	return ChangeResources{Changes: changes}
}

// put applies one resource as the CLI does.
func (c *cluster) put(kind, namespace, name string, spec any) {
	c.t.Helper()
	c.mustApply(KindChangeResources, cliActor, batch(putChange(kind, namespace, name, spec)))
}

// del deletes one resource as the CLI does.
func (c *cluster) del(kind, namespace, name string) {
	c.t.Helper()
	c.mustApply(KindChangeResources, cliActor, batch(deleteChange(kind, namespace, name)))
}

// refuseChange applies one change of the CLI that must be refused.
func (c *cluster) refuseChange(change ResourceChange, wantError string) {
	c.t.Helper()
	c.refuse(KindChangeResources, cliActor, batch(change), wantError)
}

// encryptedValue is a secret of one key, "value".
func encryptedValue(encrypted Encrypted, digest string) EncryptedSecret {
	return EncryptedSecret{Data: map[string]Encrypted{"value": encrypted}, Digest: digest}
}

func TestBootstrapMakesHostZeroTheActiveVoter(t *testing.T) {
	c := newCluster(t)
	host := c.state.Hosts[0]
	if host.Phase != Active || !host.Voter {
		t.Errorf("host 0: %+v", host)
	}
	if c.state.ClusterName != "prod" || c.state.Keys == nil || host.ServerChecked {
		t.Errorf("cluster %q, keys %+v, host %+v", c.state.ClusterName, c.state.Keys, host)
	}
	c.refuse(KindBootstrap, LeaderActor, Bootstrap{}, "bootstrapped already")
}

func TestTheFirstHostIsAControlPlaneHostWithValidRoles(t *testing.T) {
	tests := []struct {
		name  string
		roles hostrole.Set
		want  string
	}{
		{"no control plane", hostrole.DefaultJoin, "the first host has the roles worker,ingress; it needs control-plane"},
		{"out of order", hostrole.Set{hostrole.Worker, hostrole.ControlPlane}, "not in the order"},
		{"control plane only", hostrole.Set{hostrole.ControlPlane}, ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := &cluster{t: t, state: &State{}}
			host := testHost(0)
			host.Roles = test.roles
			bootstrap := Bootstrap{
				Keys:        ClusterKeys{CACertificate: []byte("ca"), CAKey: Encrypted("key"), SealingKey: Encrypted("age")},
				ClusterName: "prod",
				Host:        host,
			}
			if test.want == "" {
				c.mustApply(KindBootstrap, LeaderActor, bootstrap)
				return
			}
			c.refuse(KindBootstrap, LeaderActor, bootstrap, test.want)
		})
	}
}

func TestTheClusterNameIsADNSLabel(t *testing.T) {
	c := &cluster{t: t, state: &State{}}
	c.refuse(KindBootstrap, LeaderActor, Bootstrap{
		Keys:        ClusterKeys{CACertificate: []byte("ca"), CAKey: Encrypted("key"), SealingKey: Encrypted("age")},
		ClusterName: "Prod Cluster",
		Host:        testHost(0),
	}, `cluster name "Prod Cluster"`)
}

func TestCLIActorsCarryTheirName(t *testing.T) {
	if actor := CLIActor("alice"); actor != "cli/alice" || !IsCLIActor(actor) {
		t.Errorf("CLIActor(alice) = %q", actor)
	}
	for _, actor := range []string{"cli", "cli/", "host/0", LeaderActor, ""} {
		if IsCLIActor(actor) {
			t.Errorf("%q counts as the CLI", actor)
		}
	}
}

func TestNothingButTheBootstrapRunsOnAnEmptyState(t *testing.T) {
	state := &State{}
	cmd, err := NewCommand(KindSetCLICutoff, testTime, HostActor(0), SetCLICutoff{Cutoff: testTime})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.Apply(1, cmd); !errors.Is(err, ErrNotBootstrapped) {
		t.Errorf("want ErrNotBootstrapped, got %v", err)
	}
	if state.Index != 1 {
		t.Errorf("the index must follow the log even for refused commands: %d", state.Index)
	}
}

func TestAnEntryTheAgentCannotReadLeavesTheStateAndItsIndex(t *testing.T) {
	c := newCluster(t)
	before, index := c.encoded(), c.state.Index
	payload := `"payload":{"name":"alice","serial":"a1"}`
	for _, test := range []struct {
		name, entry string
	}{
		{"not JSON", `{"version":`},
		{"a newer version", fmt.Sprintf(`{"version":%d,"kind":"add-user",%s}`, CommandVersion+1, payload)},
		{"an older version", fmt.Sprintf(`{"version":%d,"kind":"add-user",%s}`, CommandVersion-2, payload)},
		{"no version", `{"kind":"add-user",` + payload + `}`},
		{"an unknown kind", fmt.Sprintf(`{"version":%d,"kind":"make-coffee",%s}`, CommandVersion, payload)},
	} {
		if _, err := c.state.ApplyEntry(index+1, []byte(test.entry)); !errors.Is(err, ErrUnreadable) {
			t.Errorf("%s: want ErrUnreadable, got %v", test.name, err)
		}
		if c.state.Index != index || c.encoded() != before {
			t.Errorf("%s: the state changed, index %d", test.name, c.state.Index)
		}
	}
}

func TestAHostHasIPv4AddressesAndAServer(t *testing.T) {
	for name, change := range map[string]func(*Host){
		"no IPv4 underlay address": func(host *Host) { host.Underlay = netip.MustParseAddr("2001:db8::2") },
		"no IPv4 public address":   func(host *Host) { host.PublicAddress = netip.MustParseAddr("2001:db8::3") },
		"no server ID":             func(host *Host) { host.ServerID = 0 },
	} {
		c := &cluster{t: t, state: &State{}}
		host := testHost(0)
		change(&host)
		c.refuse(KindBootstrap, LeaderActor, Bootstrap{
			Keys:        ClusterKeys{CACertificate: []byte("ca"), CAKey: Encrypted("key"), SealingKey: Encrypted("age")},
			ClusterName: "prod",
			Host:        host,
		}, name)
	}
}

func TestARefusedCommandMovesTheIndexOn(t *testing.T) {
	c := newCluster(t)
	err := c.apply(KindBootstrap, LeaderActor, Bootstrap{})
	var refusal *CommandError
	if !errors.As(err, &refusal) || errors.Is(err, ErrUnreadable) {
		t.Fatalf("want a CommandError, got %v", err)
	}
	if c.state.Index != c.index {
		t.Errorf("index %d, want %d", c.state.Index, c.index)
	}
}

func TestEveryKindOfCommandNamesTheReleaseThatBroughtIt(t *testing.T) {
	for kind, known := range commandKinds {
		if known.since == "" || buildinfo.IsDevelopment(known.since) {
			t.Errorf("%s: release %q", kind, known.since)
		}
	}
}

func TestTheLeaderWritesANewKindOnlyOnceEveryAgentReadsIt(t *testing.T) {
	c := newCluster(t)
	c.addHost(testHost(1))
	c.addHost(testHost(2))
	tests := []struct {
		name     string
		versions [3]string
		known    bool
	}{
		{"no agent reported yet", [3]string{"", "", ""}, true},
		{"every agent has the release", [3]string{"0.2.0", "0.2.1", "0.3.0"}, true},
		{"its release candidate", [3]string{"0.2.0", "0.2.0~rc1", "0.2.0"}, true},
		{"one agent is older", [3]string{"0.2.0", "0.1.0", "0.2.0"}, false},
		{"development builds", [3]string{"0.0.0~dev+abc", "dev", "0.2.0"}, true},
		{"development builds and an old release", [3]string{"0.0.0~dev+abc", "0.1.0", "0.0.0~dev+abc"}, false},
	}
	for _, test := range tests {
		for id, version := range test.versions {
			c.state.Hosts[HostID(id)].Version = version
		}
		err := c.state.CheckKnown(KindSetVolumeInitialized) // since 0.2.0
		if (err == nil) != test.known || (err != nil && !errors.Is(err, ErrNotReadByAll)) {
			t.Errorf("%s: %v", test.name, err)
		}
		if err := c.state.CheckKnown(KindBootstrap); err != nil {
			t.Errorf("%s: a kind of 0.1.0: %v", test.name, err)
		}
	}
	if err := c.state.CheckKnown("make-coffee"); err == nil {
		t.Error("an unknown kind passed")
	}
}

func TestMinVersionSkipsRemovedHostsAndDevelopmentBuilds(t *testing.T) {
	c := newCluster(t)
	c.addHost(testHost(1))
	c.addHost(testHost(2))
	c.state.Hosts[0].Version = "0.3.0"
	c.state.Hosts[1].Version = "0.0.0~dev+abc"
	c.state.Hosts[2].Version = "0.2.0"
	if got := c.state.MinVersion(); got != "0.2.0" {
		t.Errorf("MinVersion %q", got)
	}
	c.state.Hosts[2].Phase = Removed
	if got := c.state.MinVersion(); got != "0.3.0" {
		t.Errorf("MinVersion without the removed host %q", got)
	}
}

func TestAMoveCheckedAgainstAnOldPhaseIsRefused(t *testing.T) {
	c := newCluster(t)
	c.addHost(testHost(1))
	c.mustApply(KindSetHostPhase, LeaderActor, SetHostPhase{Host: 1, Phase: Active})
	// one CLI drained the host after another one checked "active"
	c.mustApply(KindSetHostPhase, cliActor, SetHostPhase{Host: 1, Phase: Draining, From: Active})
	c.refuse(KindSetHostPhase, cliActor, SetHostPhase{Host: 1, Phase: Failed, From: Active},
		"is draining now, no longer active")
}

func TestHostPhasesMoveOnlyAsAllowed(t *testing.T) {
	tests := []struct {
		name   string
		from   HostPhase
		to     HostPhase
		actor  string
		reason string // empty: allowed
	}{
		{"leader drains", Active, Draining, cliActor, ""},
		{"leader undrains", Draining, Active, LeaderActor, ""},
		{"leader declares failed", Active, Failed, LeaderActor, ""},
		{"leader takes a failed host back", Failed, Active, LeaderActor, ""},
		{"a failed host cannot take itself back", Failed, Active, HostActor(1), "only the leader"},
		{"a recovering host fails again", Recovering, Failed, LeaderActor, ""},
		{"only its operation moves a fencing host", Fencing, Fenced, LeaderActor, "only its fence-host operation"},
		{"a user enables a disabled host", Disabled, Fenced, cliActor, ""},
		{"leader fences", Failed, Fenced, LeaderActor, ""},
		{"host recovers", Fenced, Recovering, HostActor(1), ""},
		{"host is up to date", Recovering, Active, HostActor(1), ""},
		{"flapping brake", Fenced, Disabled, LeaderActor, ""},
		{"leader removes from any phase", Recovering, Removed, LeaderActor, ""},
		{"host cannot declare itself failed", Active, Failed, HostActor(1), "only the leader"},
		{"leader cannot report recovery", Fenced, Recovering, LeaderActor, "only host"},
		{"another host cannot report", Fenced, Recovering, HostActor(2), "only host"},
		{"another host cannot declare failed", Active, Failed, HostActor(2), "only the leader"},
		{"another host cannot remove", Active, Removed, HostActor(2), "only the leader"},
		{"no shortcut", Fenced, Active, HostActor(1), "cannot go from"},
		{"removed stays removed", Removed, Active, LeaderActor, "removed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := newCluster(t)
			c.addHost(testHost(1))
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
	for _, phase := range []HostPhase{Joining, Active, Draining, Failed, Fenced, Recovering, Disabled} {
		if len(phaseChanges[phase]) == 0 {
			t.Errorf("%s has no allowed change", phase)
		}
	}
}

func TestOnlyAHostReportsAboutItself(t *testing.T) {
	c := newCluster(t)
	c.addHost(testHost(1))
	pools := map[string]Pool{"vg-fast/thin": {Name: "vg-fast/thin", SizeBytes: 100 << 30, DataPercent: 81, MetadataPercent: 12}}
	c.mustApply(KindReportHost, HostActor(0), ReportHost{
		Host: 0, Version: "1.3.0", Pools: pools, DockerDown: true, RootUsedPercent: 91,
	})
	c.mustApply(KindReportHost, HostActor(1), ReportHost{Host: 1, Version: "1.2.9"})
	c.refuse(KindReportHost, HostActor(1), ReportHost{Host: 0, Version: "9.9.9"}, "only host")
	for _, pool := range []Pool{{SizeBytes: 0}, {SizeBytes: 1, DataPercent: 101}, {SizeBytes: 1, MetadataPercent: -1}} {
		c.refuse(KindReportHost, HostActor(1), ReportHost{
			Host: 1, Version: "1.2.9", Pools: map[string]Pool{"vg/thin": pool},
		}, "pool vg/thin")
	}
	c.refuse(KindReportHost, HostActor(1), ReportHost{Host: 1, Version: "1.2.9", RootUsedPercent: 120}, "root file system")
	if host := c.state.Hosts[0]; host.Pools["vg-fast/thin"].DataPercent != 81 || !host.DockerDown || host.RootUsedPercent != 91 {
		t.Errorf("host %+v", host)
	}
}

func TestTheIngressIsAnActiveHostWithACheckedServer(t *testing.T) {
	c := newCluster(t)
	c.refuse(KindSetIngress, LeaderActor, SetIngress{Host: 0}, "host node0 has no checked server")
	c.refuse(KindSetIngress, LeaderActor, SetIngress{Host: 5}, "no host 5")
	c.addHost(testHost(1))
	c.refuse(KindSetIngress, LeaderActor, SetIngress{Host: 1}, "host node1 is joining, not active")
	c.mustApply(KindSetHostPhase, LeaderActor, SetHostPhase{Host: 1, Phase: Active})
	c.mustApply(KindSetIngress, LeaderActor, SetIngress{Host: 1})
	if c.state.Ingress == nil || *c.state.Ingress != 1 {
		t.Errorf("ingress %v", c.state.Ingress)
	}

	worker := testHost(2)
	worker.Roles = hostrole.Set{hostrole.ControlPlane, hostrole.Worker}
	c.addHost(worker)
	c.mustApply(KindSetHostPhase, LeaderActor, SetHostPhase{Host: 2, Phase: Active})
	c.refuse(KindSetIngress, LeaderActor, SetIngress{Host: 2}, "host node2 has the roles control-plane,worker, not ingress")
}

func TestTheCLICutoffMovesOnlyForwardAndOnlyByAHost(t *testing.T) {
	c := newCluster(t)
	c.refuse(KindSetCLICutoff, cliActor, SetCLICutoff{Cutoff: testTime}, "only a host")
	c.refuse(KindSetCLICutoff, HostActor(0), SetCLICutoff{}, "no time")
	c.mustApply(KindSetCLICutoff, HostActor(0), SetCLICutoff{Cutoff: testTime})
	if !c.state.CLICutoff.Equal(testTime) {
		t.Errorf("cutoff %v", c.state.CLICutoff)
	}
	// a cutoff that moved back would let a revoked laptop in again
	c.refuse(KindSetCLICutoff, HostActor(0), SetCLICutoff{Cutoff: testTime.Add(-time.Hour)}, "not later")
	c.refuse(KindSetCLICutoff, HostActor(0), SetCLICutoff{Cutoff: testTime}, "not later")
	c.mustApply(KindSetCLICutoff, HostActor(0), SetCLICutoff{Cutoff: testTime.Add(time.Hour)})
}

func TestARevocationGoesAWhileAfterItsCertificateExpired(t *testing.T) {
	c := newCluster(t)
	// revoke goes by the command's time, not the clock's
	c.state.revoke("h1", testTime, testTime)
	c.state.revoke("h2", testExpiry, testTime.Add(revokedKept))
	if _, kept := c.state.Revoked["h1"]; !kept {
		t.Error("h1 went as soon as it expired; a host whose clock runs behind still takes it")
	}
	c.state.revoke("h3", testExpiry, testTime.Add(revokedKept+time.Second))
	if _, kept := c.state.Revoked["h1"]; kept {
		t.Error("h1 stays on the list long after it expired")
	}
	if len(c.state.Revoked) != 2 {
		t.Errorf("the certificates that did not expire went too: %v", c.state.Revoked)
	}
}

// hetznerToken is where testConfig finds the Hetzner token.
var hetznerToken = names.SecretKeyRef{Name: "hetzner", Key: "token"}

func testConfig() clusterspec.Config {
	return clusterspec.Config{
		Hetzner:        clusterspec.Hetzner{TokenSecretRef: hetznerToken},
		PrivateNetwork: "10.0.0.0/16",
		Ingress:        clusterspec.Ingress{FloatingIP: "203.0.113.11"},
		StorageClasses: map[string]clusterspec.StorageClass{"fast": {Pool: "vg-fast/thin"}},
	}
}

// testVolumeSpec is a volume of testConfig's class.
func testVolumeSpec() resource.VolumeSpec {
	return resource.VolumeSpec{Class: "fast", Size: 1 << 30, Replicas: 1}
}

// hetznerSecret puts the Secret of names.System that testConfig names.
func hetznerSecret() ResourceChange {
	return putChange(resource.KindSecret, names.System, hetznerToken.Name,
		EncryptedSecret{Data: map[string]Encrypted{hetznerToken.Key: encryptedOf(10)}, Digest: "d-token"})
}

// configChange puts the ClusterConfiguration.
func configChange(config clusterspec.Config) ResourceChange {
	return putChange(resource.KindClusterConfiguration, "", resource.ClusterConfigurationName, config)
}

// configure applies a configuration with the Secret it names above it,
// as one clowder apply -f of both.
func (c *cluster) configure(config clusterspec.Config) {
	c.t.Helper()
	c.mustApply(KindChangeResources, cliActor, batch(hetznerSecret(), configChange(config)))
}

func TestTheLeaderKeepsTheACMEAccountChallengesAndCertificates(t *testing.T) {
	c := newCluster(t)
	c.refuse(KindPutACMEAccount, LeaderActor, PutACMEAccount{Account: ACMEAccount{Directory: "https://acme"}},
		"needs its directory, URL and key")
	account := ACMEAccount{Directory: "https://acme", URL: "https://acme/acct/1", Key: Encrypted("k")}
	c.mustApply(KindPutACMEAccount, LeaderActor, PutACMEAccount{Account: account})
	if c.state.ACMEAccount == nil || c.state.ACMEAccount.URL != account.URL {
		t.Errorf("account %+v", c.state.ACMEAccount)
	}

	for _, refused := range []PutACMEChallenge{
		{Token: "", KeyAuthorization: ".thumb"},
		{Token: "a/b", KeyAuthorization: "a/b.thumb"},
		{Token: "abc", KeyAuthorization: "abd.thumb"},
		{Token: "abc", KeyAuthorization: "abc."},
		{Token: "abc", KeyAuthorization: "abc.th<umb"},
	} {
		c.refuse(KindPutACMEChallenge, LeaderActor, refused, "is not an HTTP-01 answer")
	}
	c.mustApply(KindPutACMEChallenge, LeaderActor, PutACMEChallenge{Token: "abc_-1", KeyAuthorization: "abc_-1.thumb"})
	if c.state.Challenges["abc_-1"] != "abc_-1.thumb" {
		t.Errorf("challenges %v", c.state.Challenges)
	}
	c.mustApply(KindDeleteACMEChallenge, LeaderActor, DeleteACMEChallenge{Token: "abc_-1"})
	if len(c.state.Challenges) != 0 {
		t.Errorf("challenges %v", c.state.Challenges)
	}

	certificate := Certificate{
		Domain: "shop.example.com", Key: Encrypted("k"), Chain: []byte("chain"),
		NotBefore: testTime, NotAfter: testTime.Add(90 * 24 * time.Hour),
	}
	upper, backwards, empty := certificate, certificate, certificate
	upper.Domain = "Shop.example.com"
	backwards.NotAfter = testTime
	empty.Chain = nil
	c.refuse(KindPutCertificate, LeaderActor, PutCertificate{Certificate: upper}, "not a domain in lowercase")
	c.refuse(KindPutCertificate, LeaderActor, PutCertificate{Certificate: backwards}, "ends before it starts")
	c.refuse(KindPutCertificate, LeaderActor, PutCertificate{Certificate: empty}, "needs its key and chain")
	c.mustApply(KindPutCertificate, LeaderActor, PutCertificate{Certificate: certificate})
	if got := c.state.Certificates["shop.example.com"]; got == nil || got.Lifetime() != 90*24*time.Hour {
		t.Errorf("certificate %+v", got)
	}
	c.mustApply(KindDeleteCertificate, LeaderActor, DeleteCertificate{Domain: "shop.example.com"})
	if len(c.state.Certificates) != 0 {
		t.Errorf("certificates %v", c.state.Certificates)
	}
}

func TestWaitingSinceStartsWithTheFirstReasonAndEndsWithTheLast(t *testing.T) {
	c := newCluster(t)
	c.putShop(shopSpec())
	wait := func(reason string, at time.Time) {
		c.t.Helper()
		cmd, err := NewCommand(KindSetStackWaitReason, at, LeaderActor, SetStackWaitReason{ID: "default/shop", Reason: reason})
		if err != nil {
			t.Fatal(err)
		}
		c.index++
		if _, err := c.state.Apply(c.index, cmd); err != nil {
			t.Fatal(err)
		}
	}
	wait("no room", testTime)
	wait("no room on node0", testTime.Add(time.Hour)) // the text changed, the wait did not
	if since := c.state.Stacks["default/shop"].WaitingSince; !since.Equal(testTime) {
		t.Errorf("waiting since %v", since)
	}
	wait("", testTime.Add(2*time.Hour))
	if since := c.state.Stacks["default/shop"].WaitingSince; !since.IsZero() {
		t.Errorf("waiting since %v after the wait ended", since)
	}
}

func TestAnAlertKeepsItsKeyAndSinceWhenItGetsWorse(t *testing.T) {
	c := newCluster(t)
	warning := PutAlert{Key: "pool-use/node0/vg/thin/data", Kind: "pool-use", Severity: "warning", Subject: "s", Message: "m"}
	c.refuse(KindPutAlert, LeaderActor, PutAlert{Key: "k", Kind: "x", Subject: "s", Severity: "loud"}, `severity "loud"`)
	c.refuse(KindPutAlert, LeaderActor, PutAlert{Severity: "info"}, "needs a key")
	c.mustApply(KindPutAlert, LeaderActor, warning)
	critical := warning
	critical.Severity = "critical"
	cmd, err := NewCommand(KindPutAlert, testTime.Add(time.Hour), LeaderActor, critical)
	if err != nil {
		t.Fatal(err)
	}
	c.index++
	if _, err := c.state.Apply(c.index, cmd); err != nil {
		t.Fatal(err)
	}
	if alert := c.state.Alerts[warning.Key]; alert.Severity != "critical" || !alert.Since.Equal(testTime) || len(c.state.Alerts) != 1 {
		t.Errorf("alerts %+v", c.state.Alerts)
	}
	c.mustApply(KindResolveAlert, LeaderActor, ResolveAlert{Key: warning.Key})
	if len(c.state.Alerts) != 0 {
		t.Errorf("alerts %+v", c.state.Alerts)
	}
}

func TestAWaitReasonChangesNothingElse(t *testing.T) {
	c := newCluster(t)
	c.putShop(shopSpec())
	host := HostID(0)
	c.mustApply(KindSetStackStatus, LeaderActor, SetStackStatus{PlannedAt: c.index, ID: "default/shop", Status: StackStarting, Host: &host})
	c.mustApply(KindSetStackStatus, HostActor(0), SetStackStatus{ID: "default/shop", Status: StackRunning, Host: &host, Generation: 1})

	// a running stack whose new volume fits nowhere keeps running where it is
	c.mustApply(KindSetStackWaitReason, LeaderActor, SetStackWaitReason{ID: "default/shop", Reason: "volume data fits no pool"})
	stack := c.state.Stacks["default/shop"]
	if stack.WaitReason != "volume data fits no pool" || stack.Status != StackRunning || *stack.Host != host {
		t.Errorf("stack %+v", stack)
	}
	c.mustApply(KindSetStackWaitReason, LeaderActor, SetStackWaitReason{ID: "default/shop"})
	if stack.WaitReason != "" {
		t.Errorf("an empty reason left %q", stack.WaitReason)
	}
	c.refuse(KindSetStackWaitReason, LeaderActor, SetStackWaitReason{ID: "default/gone", Reason: "x"}, "no stack default/gone")
}

func TestAVolumeRecordKeepsItsInvariantsAndOwnMinor(t *testing.T) {
	c := newCluster(t)
	c.configure(testConfig())
	for _, name := range []string{"pgdata", "cache"} {
		c.put(resource.KindVolume, "default", name, testVolumeSpec())
	}
	minor := 5
	volume := *c.state.Volumes["default/pgdata"]
	volume.Minor, volume.Copies, volume.Status = &minor, []VolumeCopy{{Host: 0, Class: "fast"}}, VolumeOK
	c.mustApply(KindPutVolume, LeaderActor, PutVolume{PlannedAt: c.index, Volume: volume})

	other := *c.state.Volumes["default/cache"]
	other.Minor, other.Copies, other.Status = &minor, volume.Copies, VolumeOK
	c.refuse(KindPutVolume, LeaderActor, PutVolume{PlannedAt: c.index, Volume: other}, "minor 5 belongs to default/pgdata")
	orphan := volume
	orphan.Name = "blog"
	c.refuse(KindPutVolume, LeaderActor, PutVolume{PlannedAt: c.index, Volume: orphan}, "no volume default/blog")
	tooOld := volume
	tooOld.DataGeneration = names.MaxDataGeneration + 1
	c.refuse(KindPutVolume, LeaderActor, PutVolume{PlannedAt: c.index, Volume: tooOld}, "data generation 1000000 is not from 0 to 999999")
	older := volume
	older.DataGeneration = 0
	c.refuse(KindPutVolume, LeaderActor, PutVolume{PlannedAt: c.index, Volume: older}, "older")
	tooHigh := 1000
	other.Minor = &tooHigh
	c.refuse(KindPutVolume, LeaderActor, PutVolume{PlannedAt: c.index, Volume: other}, "not from 0 to 999")
	twice := volume
	twice.Copies = []VolumeCopy{{Host: 0, Class: "fast"}, {Host: 0, Class: "fast"}}
	c.refuse(KindPutVolume, LeaderActor, PutVolume{PlannedAt: c.index, Volume: twice}, "two copies on host 0")
	respec := volume
	respec.Spec.Replicas = 2
	c.refuse(KindPutVolume, LeaderActor, PutVolume{PlannedAt: c.index, Volume: respec}, "its spec changes only with an apply")
	strange := volume
	strange.Status = "melting"
	c.refuse(KindPutVolume, LeaderActor, PutVolume{PlannedAt: c.index, Volume: strange}, "unknown status")
	withoutMinor := volume
	withoutMinor.Minor = nil
	c.refuse(KindPutVolume, LeaderActor, PutVolume{PlannedAt: c.index, Volume: withoutMinor}, "a DRBD minor exactly when")
}

func TestOnlyACopysHostMarksItsGenerationInitialized(t *testing.T) {
	c := newCluster(t)
	c.configure(testConfig())
	c.addHost(testHost(1))
	c.mustApply(KindSetHostPhase, LeaderActor, SetHostPhase{Host: 1, Phase: Active})
	spec := testVolumeSpec()
	spec.Replicas = 2
	c.put(resource.KindVolume, "default", "pgdata", spec)
	c.refuse(KindSetVolumeInitialized, HostActor(0), SetVolumeInitialized{Volume: "default/pgdata", DataGeneration: 1},
		"host/0 has no copy")
	minor := 5
	volume := *c.state.Volumes["default/pgdata"]
	volume.Minor, volume.Status = &minor, VolumeOK
	volume.Copies = []VolumeCopy{{Host: 0, Class: "fast"}, {Host: 1, Tiebreaker: true}}
	c.mustApply(KindPutVolume, LeaderActor, PutVolume{PlannedAt: c.index, Volume: volume})

	c.refuse(KindSetVolumeInitialized, HostActor(1), SetVolumeInitialized{Volume: "default/pgdata", DataGeneration: 1},
		"host/1 has no copy of volume default/pgdata with a disk")
	c.refuse(KindSetVolumeInitialized, HostActor(0), SetVolumeInitialized{Volume: "default/pgdata", DataGeneration: 2},
		"in data generation 1, not 2")
	c.refuse(KindSetVolumeInitialized, HostActor(0), SetVolumeInitialized{Volume: "default/blog", DataGeneration: 1},
		"no volume default/blog")
	c.mustApply(KindSetVolumeInitialized, HostActor(0), SetVolumeInitialized{Volume: "default/pgdata", DataGeneration: 1})
	if !c.state.Volumes["default/pgdata"].Initialized {
		t.Fatal("not initialized")
	}

	// the placement's record keeps it, and cannot set it
	moved := volume
	moved.Copies = []VolumeCopy{{Host: 0, Class: "fast"}}
	c.mustApply(KindPutVolume, LeaderActor, PutVolume{PlannedAt: c.index, Volume: moved})
	if !c.state.Volumes["default/pgdata"].Initialized {
		t.Error("a put-volume of the same generation lost it")
	}
	// a new generation is a new LV, not initialized yet
	restored := moved
	restored.DataGeneration, restored.Initialized = 2, true
	c.mustApply(KindPutVolume, LeaderActor, PutVolume{PlannedAt: c.index, Volume: restored})
	if c.state.Volumes["default/pgdata"].Initialized {
		t.Error("a new data generation came initialized")
	}
}

func TestOnlyRetiredCopiesMayGo(t *testing.T) {
	c := newCluster(t)
	c.configure(testConfig())
	c.addHost(testHost(1))
	c.mustApply(KindSetHostPhase, LeaderActor, SetHostPhase{Host: 1, Phase: Active})
	spec := testVolumeSpec()
	spec.Replicas = 2
	c.put(resource.KindVolume, "default", "pgdata", spec)
	minor := 5
	volume := *c.state.Volumes["default/pgdata"]
	volume.Minor, volume.Status = &minor, VolumeOK
	volume.Copies = []VolumeCopy{{Host: 0, Class: "fast"}, {Host: 1, Class: "fast"}}
	c.mustApply(KindPutVolume, LeaderActor, PutVolume{PlannedAt: c.index, Volume: volume})
	if len(c.state.Retired) != 0 {
		t.Fatalf("a new volume retired %v", c.state.Retired)
	}

	// the copy on host 1 leaves; the one on host 0 stays
	moved := volume
	moved.Copies = []VolumeCopy{{Host: 0, Class: "fast"}}
	c.mustApply(KindPutVolume, LeaderActor, PutVolume{PlannedAt: c.index, Volume: moved})
	// a new generation retires the copies of the old one
	restored := moved
	restored.DataGeneration = 2
	c.mustApply(KindPutVolume, LeaderActor, PutVolume{PlannedAt: c.index, Volume: restored})
	want := []RetiredCopy{
		{Volume: "default/pgdata", DataGeneration: 1, Host: 1},
		{Volume: "default/pgdata", DataGeneration: 1, Host: 0},
	}
	if !slices.Equal(c.state.Retired, want) {
		t.Errorf("retired %v, want %v", c.state.Retired, want)
	}

	// each host forgets its own, once
	c.refuse(KindForgetCopy, HostActor(0), ForgetCopy{Copy: want[0]}, "only host 1 itself")
	c.mustApply(KindForgetCopy, HostActor(1), ForgetCopy{Copy: want[0]})
	c.refuse(KindForgetCopy, HostActor(1), ForgetCopy{Copy: want[0]}, "no retired copy")

	// a deleted volume retires its copies
	c.del(resource.KindVolume, "default", "pgdata")
	if !slices.Contains(c.state.Retired, RetiredCopy{Volume: "default/pgdata", DataGeneration: 2, Host: 0}) {
		t.Errorf("retired %v after the delete", c.state.Retired)
	}
	// its minor stays taken while a host still has a retired copy: the
	// copy's resource has it there
	c.put(resource.KindVolume, "default", "other", spec)
	other := *c.state.Volumes["default/other"]
	other.Minor, other.Status = &minor, VolumeOK
	other.Copies = []VolumeCopy{{Host: 0, Class: "fast"}}
	c.refuse(KindPutVolume, LeaderActor, PutVolume{PlannedAt: c.index, Volume: other},
		"minor 5 is still the retired copies' of default/pgdata")

	c.mustApply(KindForgetCopy, HostActor(0), ForgetCopy{Copy: want[1]})
	c.mustApply(KindForgetCopy, HostActor(0), ForgetCopy{Copy: RetiredCopy{Volume: "default/pgdata", DataGeneration: 2, Host: 0}})
	if len(c.state.RetiredMinors) != 0 {
		t.Errorf("retired minors %v once every copy is gone", c.state.RetiredMinors)
	}
	c.mustApply(KindPutVolume, LeaderActor, PutVolume{PlannedAt: c.index, Volume: other})

	// a removed host forgets nothing any more: its copies leave the list
	c.state.Retired = append(c.state.Retired, RetiredCopy{Volume: "default/blog", DataGeneration: 1, Host: 1})
	c.mustApply(KindSetHostPhase, LeaderActor, SetHostPhase{Host: 1, Phase: Removed})
	if len(c.state.Retired) != 0 {
		t.Errorf("retired %v after the host was removed", c.state.Retired)
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
	c.addHost(testHost(1))
	c.mustApply(KindSetCLICutoff, HostActor(0), SetCLICutoff{Cutoff: testTime})
	c.putShop(shopSpec())

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
	copied.mustApply(KindSetHostPhase, LeaderActor, SetHostPhase{Host: 1, Phase: Active})
}

func TestSnapshotsOfUnknownVersionsAreRefused(t *testing.T) {
	var buffer bytes.Buffer
	if err := (&State{}).WriteSnapshot(&buffer); err != nil {
		t.Fatal(err)
	}
	future := bytes.Replace(gunzip(t, buffer.Bytes()), []byte(`"version":1`), []byte(`"version":2`), 1)
	if _, err := ReadSnapshot(bytes.NewReader(gzipped(t, future))); !errors.Is(err, ErrUnreadable) {
		t.Errorf("want ErrUnreadable for a newer version, got %v", err)
	}
	if _, err := ReadSnapshot(bytes.NewReader([]byte("no gzip"))); !errors.Is(err, ErrUnreadable) {
		t.Errorf("want ErrUnreadable for a broken file, got %v", err)
	}
}

func TestTwoAgentsApplyingTheSameLogAgree(t *testing.T) {
	first, second := newCluster(t), newCluster(t)
	for _, c := range []*cluster{first, second} {
		c.addHost(testHost(1))
		c.addHost(testHost(2))
		c.mustApply(KindSetCLICutoff, HostActor(0), SetCLICutoff{Cutoff: testTime})
		_ = c.apply(KindSetCLICutoff, HostActor(0), SetCLICutoff{Cutoff: testTime}) // refused on both: not later
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
	c.addHost(testHost(1))
	c.mustApply(KindSetHostPhase, cliActor, SetHostPhase{Host: 1, Phase: Removed})
	want := Revocation{At: testTime, NotAfter: testExpiry}
	if revocation := c.state.Revoked[testHost(1).CertificateSerial]; revocation != want {
		t.Errorf("the removed host's certificate is not revoked: %v", c.state.Revoked)
	}
}

func TestTheLastControlPlaneHostInServiceStays(t *testing.T) {
	const last = "the last control-plane host in service"
	c := newCluster(t)
	c.refuse(KindSetHostPhase, cliActor, SetHostPhase{Host: 0, Phase: Removed}, last)
	c.mustApply(KindSetHostPhase, cliActor, SetHostPhase{Host: 0, Phase: Draining})
	c.refuse(KindSetHostPhase, LeaderActor, SetHostPhase{Host: 0, Phase: Removed}, last)

	// with a second active control-plane host, either may go, but not both
	c.addHost(testHost(1))
	c.mustApply(KindSetHostPhase, LeaderActor, SetHostPhase{Host: 1, Phase: Active})
	c.mustApply(KindSetHostPhase, cliActor, SetHostPhase{Host: 0, Phase: Removed})
	c.refuse(KindSetHostPhase, cliActor, SetHostPhase{Host: 1, Phase: Removed}, last)
}

func TestAWorkerDoesNotKeepTheLastControlPlaneHost(t *testing.T) {
	c := newCluster(t)
	worker := testHost(1)
	worker.Roles = hostrole.Set{hostrole.Worker}
	c.addHost(worker)
	c.mustApply(KindSetHostPhase, LeaderActor, SetHostPhase{Host: 1, Phase: Active})
	c.refuse(KindSetHostPhase, cliActor, SetHostPhase{Host: 0, Phase: Removed},
		"the last control-plane host in service")
	// a host without the control plane may always go
	c.mustApply(KindSetHostPhase, cliActor, SetHostPhase{Host: 1, Phase: Removed})
}

func TestOnlyControlPlaneHostsMakeAClusterHighlyAvailable(t *testing.T) {
	c := newCluster(t)
	for id := HostID(1); id <= 2; id++ {
		worker := testHost(id)
		worker.Roles = hostrole.DefaultJoin
		c.addHost(worker)
		c.mustApply(KindSetHostPhase, LeaderActor, SetHostPhase{Host: id, Phase: Active})
	}
	if c.state.HA {
		t.Fatal("one control-plane host and two workers are highly available")
	}
	// once highly available, a worker still may go, a control-plane host not
	c.activeHost(3)
	c.activeHost(4)
	if !c.state.HA {
		t.Fatal("three control-plane hosts are not highly available")
	}
	c.mustApply(KindSetHostPhase, cliActor, SetHostPhase{Host: 1, Phase: Removed})
	c.refuse(KindSetHostPhase, cliActor, SetHostPhase{Host: 3, Phase: Removed}, "join a replacement first")
}
