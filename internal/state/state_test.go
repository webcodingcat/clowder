package state

import (
	"bytes"
	"compress/gzip"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/webcodingcat/clowder/internal/clusterspec"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/resource"
	"github.com/webcodingcat/clowder/internal/stackspec"
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
		JoinKey: SealedSecret{Value: Sealed("join"), Digest: "d-join"},
		Host:    testHost(0),
	})
	return c
}

func testHost(id HostID) Host {
	return Host{
		ID:                id,
		Name:              "node" + strconv.Itoa(int(id)),
		Underlay:          netip.AddrFrom4([4]byte{10, 0, 0, byte(id + 2)}),
		WGPublicKey:       "wg-key",
		CertificateSerial: "serial-" + strconv.Itoa(int(id)),
	}
}

// changes applies a command and returns what Apply returns.
func (c *cluster) changes(kind, actor string, payload any) ([]Change, error) {
	c.t.Helper()
	cmd, err := NewCommand(kind, testTime, actor, payload)
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
	if len(c.state.ValidJoinKeys()) != 1 || c.state.Keys == nil {
		t.Errorf("keys: %+v %v", c.state.Keys, c.state.ValidJoinKeys())
	}
	c.refuse(KindBootstrap, LeaderActor, Bootstrap{}, "bootstrapped already")
}

func TestNothingButTheBootstrapRunsOnAnEmptyState(t *testing.T) {
	state := &State{}
	cmd, err := NewCommand(KindAddUser, testTime, LeaderActor, AddUser{Name: "alice", Serial: "1"})
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
		if _, err := c.state.Apply(99, cmd); err == nil || !strings.Contains(err.Error(), test.want) {
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

func TestAMoveCheckedAgainstAnOldPhaseIsRefused(t *testing.T) {
	c := newCluster(t)
	c.mustApply(KindAddHost, LeaderActor, AddHost{Host: testHost(1)})
	c.mustApply(KindSetHostPhase, LeaderActor, SetHostPhase{Host: 1, Phase: Active})
	// a user drained the host after another one checked "active"
	c.mustApply(KindSetHostPhase, UserActor("alice"), SetHostPhase{Host: 1, Phase: Draining, From: Active})
	c.refuse(KindSetHostPhase, UserActor("bob"), SetHostPhase{Host: 1, Phase: Failed, From: Active},
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
	if got := c.state.MinVersion(); got != "1.2.9" {
		t.Errorf("MinVersion %s", got)
	}
	if host := c.state.Hosts[0]; host.Pools["vg-fast/thin"].DataPercent != 81 || !host.DockerDown || host.RootUsedPercent != 91 {
		t.Errorf("host %+v", host)
	}
}

func TestTheIngressIsAnActiveHostWithAServerRef(t *testing.T) {
	c := newCluster(t)
	c.refuse(KindSetIngress, LeaderActor, SetIngress{Host: 0}, "host node0 has no server ref")
	c.refuse(KindSetIngress, LeaderActor, SetIngress{Host: 5}, "no host 5")
	host := testHost(1)
	host.ServerRef = "4711"
	c.mustApply(KindAddHost, LeaderActor, AddHost{Host: host})
	c.refuse(KindSetIngress, LeaderActor, SetIngress{Host: 1}, "host node1 is joining, not active")
	c.mustApply(KindSetHostPhase, LeaderActor, SetHostPhase{Host: 1, Phase: Active})
	c.mustApply(KindSetIngress, LeaderActor, SetIngress{Host: 1})
	if c.state.Ingress == nil || *c.state.Ingress != 1 {
		t.Errorf("ingress %v", c.state.Ingress)
	}
}

func TestOnlyOneFirstUserAndItBringsTheRecoveryKey(t *testing.T) {
	c := newCluster(t)
	c.refuse(KindAddUser, LeaderActor, AddUser{Name: "alice", Serial: "a1", First: true}, "recovery key's recipient")
	c.mustApply(KindAddUser, LeaderActor, AddUser{Name: "alice", Serial: "a1", First: true, RecoveryRecipient: "age1first"})
	if c.state.RecoveryRecipient != "age1first" {
		t.Errorf("recovery recipient %q", c.state.RecoveryRecipient)
	}
	c.refuse(KindAddUser, LeaderActor, AddUser{Name: "bob", Serial: "b1", First: true, RecoveryRecipient: "age1b"},
		"first user already")
	c.refuse(KindAddUser, LeaderActor, AddUser{Name: "bob", Serial: "b1", RecoveryRecipient: "age1b"},
		"recovery key's recipient")
}

func TestARecoveredUserKeepsOnlyTheNewCertificate(t *testing.T) {
	c := newCluster(t)
	c.mustApply(KindAddUser, LeaderActor, AddUser{Name: "alice", Serial: "a1"})
	c.mustApply(KindAddUser, LeaderActor, AddUser{Name: "alice", Serial: "a2"})
	c.mustApply(KindAddUser, LeaderActor, AddUser{Name: "bob", Serial: "b1"})
	c.refuse(KindRecoverUser, UserActor("carol"), RecoverUser{Name: "carol", Serial: "c1"}, "no user carol")
	c.refuse(KindRecoverUser, UserActor("alice"), RecoverUser{Name: "alice", Serial: "b1"}, "used already")

	c.mustApply(KindRecoverUser, UserActor("alice"), RecoverUser{Name: "alice", Serial: "a3"})
	if serials := c.state.Users["alice"].Serials; len(serials) != 1 || serials[0] != "a3" {
		t.Errorf("alice keeps only a3: %v", serials)
	}
	for _, serial := range []string{"a1", "a2"} {
		if at, ok := c.state.Revoked[serial]; !ok || !at.Equal(testTime) {
			t.Errorf("%s not revoked at the command's time: %v", serial, at)
		}
	}
	if _, revoked := c.state.Revoked["b1"]; revoked || len(c.state.Users["bob"].Serials) != 1 {
		t.Error("bob's certificate went with alice's")
	}
}

func TestANewRecoveryKeyReplacesTheOld(t *testing.T) {
	c := newCluster(t)
	c.mustApply(KindAddUser, LeaderActor, AddUser{Name: "alice", Serial: "a1", First: true, RecoveryRecipient: "age1first"})
	c.refuse(KindSetRecoveryRecipient, UserActor("alice"), SetRecoveryRecipient{Recipient: "AGE-SECRET-KEY-1X"}, "no age key")
	c.mustApply(KindSetRecoveryRecipient, UserActor("alice"), SetRecoveryRecipient{Recipient: "age1second"})
	if c.state.RecoveryRecipient != "age1second" {
		t.Errorf("recovery recipient %q", c.state.RecoveryRecipient)
	}
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

func testConfig() clusterspec.Config {
	return clusterspec.Config{
		Name: "prod", Seeds: []string{"10.0.0.2"},
		Hoster:         clusterspec.Hoster{Type: "hetzner", Token: "hoster.token"},
		Ingress:        clusterspec.Ingress{FloatingIP: "203.0.113.11"},
		StorageClasses: map[string]clusterspec.StorageClass{"fast": {Pool: "vg-fast/thin"}},
	}
}

// testVolumeSpec is a volume of testConfig's class.
func testVolumeSpec() resource.VolumeSpec {
	return resource.VolumeSpec{Class: "fast", Size: 1 << 30, Replicas: 1}
}

// testSecrets are the values testConfig names.
func testSecrets() map[string]SealedSecret {
	return map[string]SealedSecret{"hoster.token": {Value: Sealed("sealed"), Digest: "d-token"}}
}

func TestApplyConfigCountsRevisionsAndReplacesTheSecrets(t *testing.T) {
	c := newCluster(t)
	if c.state.Config.Revision != 0 || len(c.state.ValidJoinKeys()) != 1 {
		t.Errorf("after init: config %+v, join keys %v", c.state.Config, c.state.ValidJoinKeys())
	}
	config := testConfig()
	config.JoinKeys = []clusterspec.SecretRef{"join_keys[0]", "join_keys[1]"}
	config.Registries = map[string]clusterspec.Registry{"ghcr.io": {Username: "robot", Password: `registries["ghcr.io"].password`}}
	secrets := testSecrets()
	secrets["join_keys[0]"] = SealedSecret{Value: Sealed("k1"), Digest: "d1"}
	secrets["join_keys[1]"] = SealedSecret{Value: Sealed("k2"), Digest: "d2"}
	secrets[`registries["ghcr.io"].password`] = SealedSecret{Value: Sealed("pw"), Digest: "d3"}
	c.mustApply(KindApplyConfig, UserActor("alice"), ApplyConfig{Spec: config, Secrets: secrets})
	if keys := c.state.ValidJoinKeys(); len(keys) != 2 || string(keys[1]) != "k2" {
		t.Errorf("join keys %q", keys)
	}
	if login := c.state.RegistryLogins()["ghcr.io"]; login.Username != "robot" || string(login.Password) != "pw" {
		t.Errorf("login %+v", login)
	}

	config.JoinKeys = config.JoinKeys[:1]
	c.refuse(KindApplyConfig, UserActor("alice"), ApplyConfig{Spec: config, Secrets: secrets},
		"a value for join_keys[1], which cluster.yaml does not name")
	delete(secrets, "join_keys[1]")
	c.mustApply(KindApplyConfig, UserActor("alice"), ApplyConfig{Spec: config, Secrets: secrets})
	if c.state.Config.Revision != 2 || len(c.state.ValidJoinKeys()) != 1 {
		t.Errorf("config %+v, join keys %v", c.state.Config, c.state.ValidJoinKeys())
	}
	c.refuse(KindApplyConfig, UserActor("alice"), ApplyConfig{Spec: testConfig()}, "hoster.token: the value is missing")
	c.refuse(KindApplyConfig, UserActor("alice"), ApplyConfig{
		Spec: testConfig(), Secrets: map[string]SealedSecret{"hoster.token": {Value: Sealed("sealed")}},
	}, "hoster.token: the value has no digest")
	c.refuse(KindApplyConfig, UserActor("alice"), ApplyConfig{Secrets: secrets}, "name is required")
}

func TestTheFilesNamedInClusterYAMLComeWithIt(t *testing.T) {
	c := newCluster(t)
	config := testConfig()
	config.ACME.CA = "pebble.pem"
	ca := testCertificatePEM(t)
	c.refuse(KindApplyConfig, UserActor("alice"), ApplyConfig{Spec: config, Secrets: testSecrets()},
		"file pebble.pem is named in cluster.yaml but missing")
	c.mustApply(KindApplyConfig, UserActor("alice"), ApplyConfig{
		Spec: config, Secrets: testSecrets(), Files: map[string][]byte{"pebble.pem": ca, "other.pem": ca},
	})
	files := c.state.Config.Files
	if len(files) != 1 || !bytes.Equal(files["pebble.pem"], ca) {
		t.Errorf("files %v: want only pebble.pem", files)
	}
}

// testCertificatePEM is a self-signed certificate, as acme.ca holds one.
func testCertificatePEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: testTime, NotAfter: testTime.Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestTheLeaderKeepsTheACMEAccountChallengesAndCertificates(t *testing.T) {
	c := newCluster(t)
	c.refuse(KindPutACMEAccount, LeaderActor, PutACMEAccount{Account: ACMEAccount{Directory: "https://acme"}},
		"needs its directory, URL and key")
	account := ACMEAccount{Directory: "https://acme", URL: "https://acme/acct/1", Key: Sealed("k")}
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
		Domain: "shop.example.com", Key: Sealed("k"), Chain: []byte("chain"),
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
	c.mustApply(KindSetStackStatus, LeaderActor, SetStackStatus{ID: "default/shop", Status: StackStarting, Host: &host})
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
	c.mustApply(KindApplyConfig, UserActor("alice"), ApplyConfig{Spec: testConfig(), Secrets: testSecrets()})
	for _, name := range []string{"pgdata", "cache"} {
		c.mustApply(KindPutResource, UserActor("alice"), putResource(resource.KindVolume, "default", name, testVolumeSpec()))
	}
	minor := 5
	volume := *c.state.Volumes["default/pgdata"]
	volume.Minor, volume.Copies, volume.Status = &minor, []VolumeCopy{{Host: 0, Class: "fast"}}, VolumeOK
	c.mustApply(KindPutVolume, LeaderActor, PutVolume{Volume: volume})

	other := *c.state.Volumes["default/cache"]
	other.Minor, other.Copies, other.Status = &minor, volume.Copies, VolumeOK
	c.refuse(KindPutVolume, LeaderActor, PutVolume{Volume: other}, "minor 5 belongs to default/pgdata")
	orphan := volume
	orphan.Name = "blog"
	c.refuse(KindPutVolume, LeaderActor, PutVolume{Volume: orphan}, "no volume default/blog")
	tooOld := volume
	tooOld.DataGeneration = names.MaxDataGeneration + 1
	c.refuse(KindPutVolume, LeaderActor, PutVolume{Volume: tooOld}, "data generation 1000000 is not from 0 to 999999")
	older := volume
	older.DataGeneration = 0
	c.refuse(KindPutVolume, LeaderActor, PutVolume{Volume: older}, "older")
	tooHigh := 1000
	other.Minor = &tooHigh
	c.refuse(KindPutVolume, LeaderActor, PutVolume{Volume: other}, "not from 0 to 999")
	twice := volume
	twice.Copies = []VolumeCopy{{Host: 0, Class: "fast"}, {Host: 0, Class: "fast"}}
	c.refuse(KindPutVolume, LeaderActor, PutVolume{Volume: twice}, "2 copies for 1 replicas")
	respec := volume
	respec.Spec.Replicas = 2
	c.refuse(KindPutVolume, LeaderActor, PutVolume{Volume: respec}, "its spec changes only with an apply")
	strange := volume
	strange.Status = "melting"
	c.refuse(KindPutVolume, LeaderActor, PutVolume{Volume: strange}, "unknown status")
	withoutMinor := volume
	withoutMinor.Minor = nil
	c.refuse(KindPutVolume, LeaderActor, PutVolume{Volume: withoutMinor}, "a DRBD minor exactly when")
}

func TestOnlyRetiredCopiesMayGo(t *testing.T) {
	c := newCluster(t)
	c.mustApply(KindApplyConfig, UserActor("alice"), ApplyConfig{Spec: testConfig(), Secrets: testSecrets()})
	c.mustApply(KindAddHost, LeaderActor, AddHost{Host: testHost(1)})
	c.mustApply(KindSetHostPhase, LeaderActor, SetHostPhase{Host: 1, Phase: Active})
	spec := testVolumeSpec()
	spec.Replicas = 2
	c.mustApply(KindPutResource, UserActor("alice"), putResource(resource.KindVolume, "default", "pgdata", spec))
	minor := 5
	volume := *c.state.Volumes["default/pgdata"]
	volume.Minor, volume.Status = &minor, VolumeOK
	volume.Copies = []VolumeCopy{{Host: 0, Class: "fast"}, {Host: 1, Class: "fast"}}
	c.mustApply(KindPutVolume, LeaderActor, PutVolume{Volume: volume})
	if len(c.state.Retired) != 0 {
		t.Fatalf("a new volume retired %v", c.state.Retired)
	}

	// the copy on host 1 leaves; the one on host 0 stays
	moved := volume
	moved.Copies = []VolumeCopy{{Host: 0, Class: "fast"}}
	c.mustApply(KindPutVolume, LeaderActor, PutVolume{Volume: moved})
	// a new generation retires the copies of the old one
	restored := moved
	restored.DataGeneration = 2
	c.mustApply(KindPutVolume, LeaderActor, PutVolume{Volume: restored})
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
	c.mustApply(KindDeleteResource, UserActor("alice"),
		DeleteResource{Kind: resource.KindVolume, Namespace: "default", Name: "pgdata", Owner: OwnerCLI})
	if !slices.Contains(c.state.Retired, RetiredCopy{Volume: "default/pgdata", DataGeneration: 2, Host: 0}) {
		t.Errorf("retired %v after the delete", c.state.Retired)
	}

	c.mustApply(KindForgetCopy, HostActor(0), ForgetCopy{Copy: want[1]})
	c.mustApply(KindForgetCopy, HostActor(0), ForgetCopy{Copy: RetiredCopy{Volume: "default/pgdata", DataGeneration: 2, Host: 0}})

	// a removed host forgets nothing any more: its copies leave the list
	c.state.Retired = append(c.state.Retired, RetiredCopy{Volume: "default/blog", DataGeneration: 1, Host: 1})
	c.mustApply(KindSetHostPhase, LeaderActor, SetHostPhase{Host: 1, Phase: Removed})
	if len(c.state.Retired) != 0 {
		t.Errorf("retired %v after the host was removed", c.state.Retired)
	}
}

func TestOperationsHoldTheirLocks(t *testing.T) {
	c := newCluster(t)
	c.putShop(shopSpec())
	move := Operation{ID: "01A", Kind: "move-stack", Locks: []string{"stack/default/shop", "volume/default/pgdata"}}
	c.mustApply(KindStartOperation, LeaderActor, StartOperation{Operation: move})

	changed := shopSpec()
	changed.Services["app"] = stackspec.Service{Image: "y"}
	c.refuse(KindPutResource, UserActor("alice"), putResource(resource.KindStack, "default", "shop", changed),
		"locked by operation 01A")
	c.refuse(KindRestartStack, UserActor("alice"), RestartStack{Namespace: "default", Name: "shop"}, "locked by operation 01A")
	backup := Operation{ID: "01B", Kind: "backup", Locks: []string{"volume/default/pgdata"}}
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
	for _, lock := range []string{"x", "stack/", "stack/shop", "volume/shop", "volume/shop/web/data", "volume/Shop/data"} {
		c.refuse(KindStartOperation, LeaderActor, StartOperation{Operation: Operation{
			ID: "01A", Kind: "backup", Locks: []string{lock},
		}}, "neither stack")
	}
}

func TestFailoverTakesOverLocks(t *testing.T) {
	c := newCluster(t)
	c.mustApply(KindStartOperation, LeaderActor, StartOperation{Operation: Operation{
		ID: "01A", Kind: "backup", Locks: []string{"volume/default/pgdata", "volume/default/files"},
	}})
	c.mustApply(KindStartOperation, LeaderActor, StartOperation{TakeOver: true, Operation: Operation{
		ID: "01B", Kind: "failover", Locks: []string{"volume/default/pgdata"},
	}})
	if op := c.state.Operations["01A"]; op.State != OpFailed || !strings.Contains(op.Error, "01B") {
		t.Errorf("the taken-over operation: %+v", op)
	}
	if _, held := c.state.Locks["volume/default/files"]; held {
		t.Error("the failed operation still holds its other lock")
	}
	if c.state.Locks["volume/default/pgdata"] != "01B" {
		t.Errorf("locks %v", c.state.Locks)
	}
}

func TestOnlyTheLastFinishedOperationsAreKept(t *testing.T) {
	c := newCluster(t)
	for number := range keptFinished + 5 {
		id := string(rune('A'+number/26)) + string(rune('A'+number%26))
		c.mustApply(KindStartOperation, LeaderActor, StartOperation{Operation: Operation{
			ID: id, Kind: "backup", Locks: []string{"volume/default/pgdata"},
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
