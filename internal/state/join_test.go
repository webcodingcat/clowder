package state

import (
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/webcodingcat/clowder/internal/hostrole"
	"github.com/webcodingcat/clowder/internal/resource"
)

// joiningHost is host 1 as the leader adds it with the token "t1".
func joiningHost() AddHost {
	host := testHost(1)
	host.ServerChecked = true
	return AddHost{Token: "t1", Host: host}
}

func TestATokenOpensOnceUntilItExpires(t *testing.T) {
	c := newCluster(t)
	c.mustApply(KindAddJoinToken, cliActor, joinToken("t1"))
	if token := c.state.JoinTokens["t1"]; token.Hash != testHash || !token.Expires.Equal(testTime.Add(time.Hour)) ||
		!slices.Equal(token.Roles, hostrole.DefaultInit) {
		t.Errorf("token %+v", token)
	}

	// the handler checks the expiry against the command's time
	if _, err := c.applyAt(testTime.Add(time.Hour), KindAddHost, LeaderActor, joiningHost()); err == nil ||
		!strings.Contains(err.Error(), "the join token does not open: it expired") {
		t.Errorf("an expired token: %v", err)
	}
	c.mustApply(KindAddHost, LeaderActor, joiningHost())
	if host := c.state.Hosts[1]; host.Phase != Joining || host.Voter || !host.ServerChecked {
		t.Errorf("host 1: %+v", host)
	}
	if _, ok := c.state.JoinTokens["t1"]; ok {
		t.Error("the join did not use the token up")
	}
	second := joiningHost()
	second.Host = testHost(2)
	second.Host.ServerChecked = true
	c.refuse(KindAddHost, LeaderActor, second, "it is unknown, or a host joined with it")
	second.Token = "never-made"
	c.refuse(KindAddHost, LeaderActor, second, "it is unknown")
}

func TestAJoinTokenHasLimits(t *testing.T) {
	c := newCluster(t)
	token := func(id string, ttl time.Duration) AddJoinToken {
		add := joinToken(id)
		add.TTL = ttl
		return add
	}
	c.refuse(KindAddJoinToken, cliActor, token("a", 59*time.Second), "a join token lives 1m0s to 24h0m0s")
	c.refuse(KindAddJoinToken, cliActor, token("a", 25*time.Hour), "a join token lives")
	short := joinToken("a")
	short.Hash = "short"
	c.refuse(KindAddJoinToken, cliActor, short, "no SHA-256")
	c.refuse(KindAddJoinToken, cliActor, token("", time.Hour), "has no ID")
	c.refuse(KindAddJoinToken, HostActor(0), token("a", time.Hour), "only the CLI does")
	c.mustApply(KindAddJoinToken, cliActor, token("a", time.Hour))
	c.refuse(KindAddJoinToken, cliActor, token("a", time.Hour), "exists already")
	for index := 1; index < MaxJoinTokens; index++ {
		c.mustApply(KindAddJoinToken, cliActor, token("t"+string(rune('A'+index)), time.Hour))
	}
	c.refuse(KindAddJoinToken, cliActor, token("one-more", time.Hour), "32 join tokens are open already")
}

func TestExpiredTokensGoAWhileAfter(t *testing.T) {
	c := newCluster(t)
	old := joinToken("old")
	old.TTL = time.Minute
	c.mustApply(KindAddJoinToken, cliActor, old)
	later := testTime.Add(time.Minute + joinTokenKept + time.Second)
	if _, err := c.applyAt(later, KindAddJoinToken, cliActor, joinToken("new")); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.state.JoinTokens["old"]; ok || len(c.state.JoinTokens) != 1 {
		t.Errorf("tokens %v", c.state.JoinTokens)
	}
}

func TestOnlyTheLeaderAddsAHostThatIsNew(t *testing.T) {
	c := newCluster(t)
	c.mustApply(KindAddJoinToken, cliActor, joinToken("t1"))
	c.refuse(KindAddHost, cliActor, joiningHost(), "only the leader does")
	c.refuse(KindAddHost, HostActor(0), joiningHost(), "only the leader does")
	for want, change := range map[string]func(*AddHost){
		"host ID 0 is taken":              func(add *AddHost) { add.Host.ID = 0 },
		"host ID 32 is not between":       func(add *AddHost) { add.Host.ID = MaxHosts },
		"has no checked server":           func(add *AddHost) { add.Host.ServerChecked = false },
		"has no server ID":                func(add *AddHost) { add.Host.ServerID = 0 },
		"has no IPv4 public address":      func(add *AddHost) { add.Host.PublicAddress = netip.Addr{} },
		"a host is named node0 already":   func(add *AddHost) { add.Host.Name = "node0" },
		"has the underlay 10.0.0.2":       func(add *AddHost) { add.Host.Underlay = testHost(0).Underlay },
		"serial serial-0 is used already": func(add *AddHost) { add.Host.CertificateSerial = "serial-0" },
		"has no WireGuard key":            func(add *AddHost) { add.Host.WGPublicKey = "" },
		`host name "node 2\n"`:            func(add *AddHost) { add.Host.Name = "node 2\n" },
	} {
		add := joiningHost()
		change(&add)
		c.refuse(KindAddHost, LeaderActor, add, want)
	}
}

func TestAServerBelongsToOneHost(t *testing.T) {
	c := newCluster(t)
	c.mustApply(KindAddJoinToken, cliActor, joinToken("t1"))
	add := joiningHost()
	add.Host.ServerID = 40
	c.refuse(KindAddHost, LeaderActor, add, "host node0 has the server 40 already")

	c.refuse(KindConfirmServer, LeaderActor, ConfirmServer{Host: 0, ServerID: 49}, "has the server 40, not 49")
	c.refuse(KindConfirmServer, HostActor(0), ConfirmServer{Host: 0, ServerID: 40}, "only the leader does")
	c.mustApply(KindConfirmServer, LeaderActor, ConfirmServer{Host: 0, ServerID: 40})
	if !c.state.Hosts[0].ServerChecked {
		t.Error("host 0's server is not checked")
	}
	if err := c.apply(KindConfirmServer, LeaderActor, ConfirmServer{Host: 0, ServerID: 40}); err != nil {
		t.Errorf("confirming twice: %v", err)
	}
	c.mustApply(KindAddHost, LeaderActor, joiningHost())
	c.state.Hosts[1].ServerID, c.state.Hosts[1].ServerChecked = 40, false
	c.refuse(KindConfirmServer, LeaderActor, ConfirmServer{Host: 1, ServerID: 40}, "the same server 40")
}

func TestAnUnderlayIsInThePrivateNetwork(t *testing.T) {
	c := newCluster(t)
	c.configure(testConfig())
	c.mustApply(KindAddJoinToken, cliActor, joinToken("t1"))
	add := joiningHost()
	add.Host.Underlay = netip.MustParseAddr("192.168.0.3")
	c.refuse(KindAddHost, LeaderActor, add, "host node1 has the underlay 192.168.0.3, outside privateNetwork 10.0.0.0/16")

	config := testConfig()
	config.PrivateNetwork = "10.1.0.0/16"
	c.refuseChange(configChange(config), "host node0 has the underlay 10.0.0.2, outside privateNetwork 10.1.0.0/16")
}

// activeHosts joins hosts 1 to last and makes them active.
func (c *cluster) activeHosts(last HostID) {
	c.t.Helper()
	for id := HostID(1); id <= last; id++ {
		c.activeHost(id)
	}
}

// activeHost joins a host and makes it active.
func (c *cluster) activeHost(id HostID) {
	c.t.Helper()
	c.addHost(testHost(id))
	c.mustApply(KindSetHostPhase, LeaderActor, SetHostPhase{Host: id, Phase: Active})
}

func TestASecondHostMayGoBeforeTheClusterIsHighlyAvailable(t *testing.T) {
	c := newCluster(t)
	c.addHost(testHost(1))
	c.mustApply(KindSetHostPhase, LeaderActor, SetHostPhase{Host: 1, Phase: Active, From: Joining})
	if c.state.HA {
		t.Error("two active hosts are highly available")
	}
	c.mustApply(KindSetHostPhase, cliActor, SetHostPhase{Host: 1, Phase: Removed})
}

func TestAJoinTokenNamesValidRoles(t *testing.T) {
	tests := []struct {
		name  string
		roles hostrole.Set
	}{
		{"none", nil},
		{"unknown", hostrole.Set{"database"}},
		{"twice", hostrole.Set{hostrole.Worker, hostrole.Worker}},
		{"out of order", hostrole.Set{hostrole.Ingress, hostrole.Worker}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := newCluster(t)
			token := joinToken("t1")
			token.Roles = test.roles
			c.refuse(KindAddJoinToken, cliActor, token, "join token t1")
		})
	}
}

func TestAHostJoinsOnlyWithRolesItsTokenAllows(t *testing.T) {
	c := newCluster(t)
	token := joinToken("t1")
	token.Roles = hostrole.DefaultJoin
	c.mustApply(KindAddJoinToken, cliActor, token)
	add := joiningHost()
	c.refuse(KindAddHost, LeaderActor, add,
		"the join token allows the roles worker,ingress, not control-plane,worker,ingress")
	add.Host.Roles = hostrole.Set{"database"}
	c.refuse(KindAddHost, LeaderActor, add, "the join token allows")
	add.Host.Roles = hostrole.Set{hostrole.Worker}
	c.mustApply(KindAddHost, LeaderActor, add)
	if roles := c.state.Hosts[1].Roles; !slices.Equal(roles, hostrole.Set{hostrole.Worker}) {
		t.Errorf("host 1 has the roles %v", roles)
	}
}

func TestAHostNeedsValidRoles(t *testing.T) {
	c := newCluster(t)
	c.mustApply(KindAddJoinToken, cliActor, joinToken("t1"))
	add := joiningHost()
	add.Host.Roles = hostrole.Set{hostrole.Worker, hostrole.ControlPlane}
	c.refuse(KindAddHost, LeaderActor, add, "host node1: roles worker,control-plane are not in the order")
}

func TestAHighlyAvailableClusterKeepsThreeHosts(t *testing.T) {
	c := newCluster(t)
	c.activeHosts(2)
	if !c.state.HA {
		t.Fatal("three active hosts are not highly available")
	}
	c.refuse(KindSetHostPhase, cliActor, SetHostPhase{Host: 2, Phase: Removed},
		"2 control-plane hosts would be left in service, and a highly available cluster keeps 3; join a replacement first")
	// a dead host too: a replacement joins first
	c.mustApply(KindSetHostPhase, LeaderActor, SetHostPhase{Host: 2, Phase: Failed})
	c.refuse(KindSetHostPhase, cliActor, SetHostPhase{Host: 2, Phase: Removed}, "join a replacement first")
	c.activeHost(3)
	c.mustApply(KindSetHostPhase, cliActor, SetHostPhase{Host: 2, Phase: Removed})
	if !c.state.HA {
		t.Error("the cluster is no longer highly available")
	}
}

func TestAHostWithAStackOrACopyStays(t *testing.T) {
	c := newCluster(t)
	c.activeHosts(1)
	c.putShop(shopSpec())
	host := HostID(1)
	c.mustApply(KindSetStackStatus, LeaderActor, SetStackStatus{PlannedAt: c.index, ID: "default/shop", Status: StackStarting, Host: &host})
	c.refuse(KindSetHostPhase, cliActor, SetHostPhase{Host: 1, Phase: Removed}, "host node1 still has stack default/shop")
	c.del(resource.KindStack, "default", "shop")
	c.mustApply(KindSetHostPhase, cliActor, SetHostPhase{Host: 1, Phase: Removed})
}

func TestARemovedHostsIDIsFreeOnceEveryHostForgotIt(t *testing.T) {
	c := newCluster(t)
	c.activeHosts(3)
	c.mustApply(KindSetHostPhase, cliActor, SetHostPhase{Host: 1, Phase: Removed})
	if waiting := c.state.Forgetting[1]; !slices.Equal(waiting, []HostID{0, 2, 3}) {
		t.Errorf("waiting for %v", waiting)
	}
	if id, _ := c.state.FreeHostID(); id != 4 {
		t.Errorf("the free ID is %d before the others forgot host 1", id)
	}
	c.refuse(KindReportPeerForgotten, HostActor(0), ReportPeerForgotten{Host: 2, Peer: 1}, "only host 2 itself reports")
	c.mustApply(KindReportPeerForgotten, HostActor(0), ReportPeerForgotten{Host: 0, Peer: 1})
	c.mustApply(KindReportPeerForgotten, HostActor(2), ReportPeerForgotten{Host: 2, Peer: 1})
	// a host that is removed meanwhile forgets nothing any more
	c.activeHost(4)
	c.mustApply(KindSetHostPhase, cliActor, SetHostPhase{Host: 3, Phase: Removed})
	if _, waiting := c.state.Forgetting[1]; waiting {
		t.Errorf("still waiting for %v", c.state.Forgetting)
	}
	if id, _ := c.state.FreeHostID(); id != 1 {
		t.Errorf("the free ID is %d, want 1", id)
	}
	// the new host takes the ID and the place of the removed one
	again := testHost(1)
	again.Name, again.CertificateSerial = "node1-new", "serial-1-new"
	c.addHost(again)
	if host := c.state.Hosts[1]; host.Name != "node1-new" || host.Phase != Joining {
		t.Errorf("host 1: %+v", host)
	}
}

func TestOnlyTheLeaderRecordsAVoter(t *testing.T) {
	c := newCluster(t)
	c.activeHosts(1)
	c.refuse(KindSetHostVoter, cliActor, SetHostVoter{Host: 1, Voter: true}, "only the leader does")
	c.mustApply(KindSetHostVoter, LeaderActor, SetHostVoter{Host: 1, Voter: true})
	if !c.state.Hosts[1].Voter {
		t.Error("host 1 does not vote")
	}
	c.refuse(KindSetHostVoter, LeaderActor, SetHostVoter{Host: 7, Voter: true}, "no host 7")
}
