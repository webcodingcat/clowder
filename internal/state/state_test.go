package state

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"

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

// applyAt applies a command of the given time. Every command of the tests
// comes through here, so here the tests check that no command changes a
// clone taken before it. A clone shares the specs and encrypted values
// with the state (State.Clone), so a handler that changed one of them in
// place would change the loops' shared copy under them.
func (c *cluster) applyAt(at time.Time, kind, actor string, payload any) ([]Change, error) {
	c.t.Helper()
	cmd, err := NewCommand(kind, at, actor, payload)
	if err != nil {
		c.t.Fatal(err)
	}
	clone := c.state.Clone()
	cloned := encodedState(c.t, clone)
	c.index++
	changes, err := c.state.Apply(c.index, cmd)
	if after := encodedState(c.t, clone); after != cloned {
		c.t.Errorf("%s changed a clone taken before it:\nbefore %s\nafter  %s", kind, cloned, after)
	}
	return changes, err
}

// encodedState returns a state as JSON.
func encodedState(t *testing.T, state *State) string {
	t.Helper()
	encoded, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// apply applies a command and returns Apply's error.
func (c *cluster) apply(kind, actor string, payload any) error {
	c.t.Helper()
	_, err := c.changes(kind, actor, payload)
	return err
}

// mustApply applies a command that must pass, so it skips the check of
// changes that a refusal leaves the state as it was.
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
