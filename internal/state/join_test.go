package state

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/webcodingcat/clowder/internal/resource"
)

// joiningHost is host 1 as the leader adds it with the token "t1".
func joiningHost() AddHost {
	host := testHost(1)
	host.ServerRef, host.ServerRefChecked = "srv-1", true
	return AddHost{Token: "t1", Host: host}
}

func TestATokenOpensOnceUntilItExpires(t *testing.T) {
	c := newCluster(t)
	c.mustApply(KindAddJoinToken, UserActor("alice"), AddJoinToken{ID: "t1", Hash: testHash, TTL: time.Hour})
	if token := c.state.JoinTokens["t1"]; token.Hash != testHash || !token.Expires.Equal(testTime.Add(time.Hour)) {
		t.Errorf("token %+v", token)
	}

	// the handler checks the expiry against the command's time
	if _, err := c.applyAt(testTime.Add(time.Hour), KindAddHost, LeaderActor, joiningHost()); err == nil ||
		!strings.Contains(err.Error(), "the join token does not open: it expired") {
		t.Errorf("an expired token: %v", err)
	}
	c.mustApply(KindAddHost, LeaderActor, joiningHost())
	if host := c.state.Hosts[1]; host.Phase != Joining || host.Voter || !host.ServerRefChecked {
		t.Errorf("host 1: %+v", host)
	}
	if _, ok := c.state.JoinTokens["t1"]; ok {
		t.Error("the join did not use the token up")
	}
	second := joiningHost()
	second.Host = testHost(2)
	second.Host.ServerRef, second.Host.ServerRefChecked = "srv-2", true
	c.refuse(KindAddHost, LeaderActor, second, "it is unknown, or a host joined with it")
	second.Token = "never-made"
	c.refuse(KindAddHost, LeaderActor, second, "it is unknown")
}

func TestAJoinTokenHasLimits(t *testing.T) {
	c := newCluster(t)
	token := func(id string, ttl time.Duration) AddJoinToken {
		return AddJoinToken{ID: id, Hash: testHash, TTL: ttl}
	}
	c.refuse(KindAddJoinToken, UserActor("alice"), token("a", 59*time.Second), "a join token lives 1m0s to 24h0m0s")
	c.refuse(KindAddJoinToken, UserActor("alice"), token("a", 25*time.Hour), "a join token lives")
	c.refuse(KindAddJoinToken, UserActor("alice"), AddJoinToken{ID: "a", Hash: "short", TTL: time.Hour}, "no SHA-256")
	c.refuse(KindAddJoinToken, UserActor("alice"), token("", time.Hour), "has no ID")
	c.refuse(KindAddJoinToken, HostActor(0), token("a", time.Hour), "only a user does")
	c.mustApply(KindAddJoinToken, UserActor("alice"), token("a", time.Hour))
	c.refuse(KindAddJoinToken, UserActor("alice"), token("a", time.Hour), "exists already")
	for index := 1; index < MaxJoinTokens; index++ {
		c.mustApply(KindAddJoinToken, UserActor("alice"), token("t"+string(rune('A'+index)), time.Hour))
	}
	c.refuse(KindAddJoinToken, UserActor("alice"), token("one-more", time.Hour), "32 join tokens are open already")
}

func TestExpiredTokensGoAWhileAfter(t *testing.T) {
	c := newCluster(t)
	c.mustApply(KindAddJoinToken, UserActor("alice"), AddJoinToken{ID: "old", Hash: testHash, TTL: time.Minute})
	later := testTime.Add(time.Minute + joinTokenKept + time.Second)
	if _, err := c.applyAt(later, KindAddJoinToken, UserActor("alice"),
		AddJoinToken{ID: "new", Hash: testHash, TTL: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.state.JoinTokens["old"]; ok || len(c.state.JoinTokens) != 1 {
		t.Errorf("tokens %v", c.state.JoinTokens)
	}
}

func TestOnlyTheLeaderAddsAHostThatIsNew(t *testing.T) {
	c := newCluster(t)
	c.mustApply(KindAddJoinToken, UserActor("alice"), AddJoinToken{ID: "t1", Hash: testHash, TTL: time.Hour})
	c.refuse(KindAddHost, UserActor("alice"), joiningHost(), "only the leader does")
	c.refuse(KindAddHost, HostActor(0), joiningHost(), "only the leader does")
	for want, change := range map[string]func(*AddHost){
		"host ID 0 is taken":              func(add *AddHost) { add.Host.ID = 0 },
		"host ID 32 is not between":       func(add *AddHost) { add.Host.ID = MaxHosts },
		"has no checked server ref":       func(add *AddHost) { add.Host.ServerRefChecked = false },
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

func TestAServerRefBelongsToOneHost(t *testing.T) {
	c := newCluster(t)
	c.state.Hosts[0].ServerRef = "srv-0"
	c.mustApply(KindAddJoinToken, UserActor("alice"), AddJoinToken{ID: "t1", Hash: testHash, TTL: time.Hour})
	add := joiningHost()
	add.Host.ServerRef = "srv-0"
	c.refuse(KindAddHost, LeaderActor, add, "host node0 has the server ref srv-0 already")

	c.refuse(KindConfirmServerRef, LeaderActor, ConfirmServerRef{Host: 0, ServerRef: "srv-9"}, `has the server ref "srv-0", not "srv-9"`)
	c.refuse(KindConfirmServerRef, HostActor(0), ConfirmServerRef{Host: 0, ServerRef: "srv-0"}, "only the leader does")
	c.mustApply(KindConfirmServerRef, LeaderActor, ConfirmServerRef{Host: 0, ServerRef: "srv-0"})
	if !c.state.Hosts[0].ServerRefChecked {
		t.Error("host 0's server ref is not checked")
	}
	if err := c.apply(KindConfirmServerRef, LeaderActor, ConfirmServerRef{Host: 0, ServerRef: "srv-0"}); err != nil {
		t.Errorf("confirming twice: %v", err)
	}
	c.mustApply(KindAddHost, LeaderActor, joiningHost())
	c.state.Hosts[1].ServerRef, c.state.Hosts[1].ServerRefChecked = "srv-0", false
	c.refuse(KindConfirmServerRef, LeaderActor, ConfirmServerRef{Host: 1, ServerRef: "srv-0"}, "the same server ref srv-0")
}

func TestTheBootstrapKeyOpensOnlyUntilTheUserIsIn(t *testing.T) {
	c := newCluster(t)
	c.mustApply(KindAddUser, UserActor("alice"), addUser("alice", "u1"))
	if c.state.BootstrapKey != nil {
		t.Errorf("the bootstrap key is still there: %q", c.state.BootstrapKey)
	}
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

func TestAPassiveSecondHostMayGoBeforeTheClusterIsHighlyAvailable(t *testing.T) {
	c := newCluster(t)
	c.addHost(testHost(1))
	c.mustApply(KindSetHostPhase, LeaderActor, SetHostPhase{Host: 1, Phase: Passive, From: Joining})
	if c.state.HA {
		t.Error("one active host and a passive one are highly available")
	}
	c.mustApply(KindSetHostPhase, UserActor("alice"), SetHostPhase{Host: 1, Phase: Removed})
}

func TestAHighlyAvailableClusterKeepsThreeHosts(t *testing.T) {
	c := newCluster(t)
	c.activeHosts(2)
	if !c.state.HA {
		t.Fatal("three active hosts are not highly available")
	}
	c.refuse(KindSetHostPhase, UserActor("alice"), SetHostPhase{Host: 2, Phase: Removed},
		"2 hosts would be left to run stacks, and a highly available cluster keeps 3; join a replacement first")
	// a dead host too: a replacement joins first
	c.mustApply(KindSetHostPhase, LeaderActor, SetHostPhase{Host: 2, Phase: Failed})
	c.refuse(KindSetHostPhase, UserActor("alice"), SetHostPhase{Host: 2, Phase: Removed}, "join a replacement first")
	c.activeHost(3)
	c.mustApply(KindSetHostPhase, UserActor("alice"), SetHostPhase{Host: 2, Phase: Removed})
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
	c.refuse(KindSetHostPhase, UserActor("alice"), SetHostPhase{Host: 1, Phase: Removed}, "host node1 still has stack default/shop")
	c.mustApply(KindDeleteResource, UserActor("alice"), DeleteResource{Kind: resource.KindStack, Namespace: "default", Name: "shop", Owner: OwnerCLI})
	c.mustApply(KindSetHostPhase, UserActor("alice"), SetHostPhase{Host: 1, Phase: Removed})
}

func TestARemovedHostsIDIsFreeOnceEveryHostForgotIt(t *testing.T) {
	c := newCluster(t)
	c.activeHosts(3)
	c.mustApply(KindSetHostPhase, UserActor("alice"), SetHostPhase{Host: 1, Phase: Removed})
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
	c.mustApply(KindSetHostPhase, UserActor("alice"), SetHostPhase{Host: 3, Phase: Removed})
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
	c.refuse(KindSetHostVoter, UserActor("alice"), SetHostVoter{Host: 1, Voter: true}, "only the leader does")
	c.mustApply(KindSetHostVoter, LeaderActor, SetHostVoter{Host: 1, Voter: true})
	if !c.state.Hosts[1].Voter {
		t.Error("host 1 does not vote")
	}
	c.refuse(KindSetHostVoter, LeaderActor, SetHostVoter{Host: 7, Voter: true}, "no host 7")
}
