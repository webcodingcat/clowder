package state

import (
	"net/netip"
	"slices"
	"testing"

	"github.com/webcodingcat/clowder/internal/hostrole"
	"github.com/webcodingcat/clowder/internal/resource"
)

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

func TestThePhasesSayWhetherAHostServesRunsStacksAndIsUp(t *testing.T) {
	tests := []struct {
		phase       HostPhase
		inService   bool
		keepsStacks bool
		up          bool
	}{
		{Joining, false, false, false},
		{Active, true, true, true},
		{Draining, true, true, true},
		{Failed, false, true, false},
		{Fencing, false, true, false},
		{Fenced, false, false, false},
		{Recovering, false, false, true},
		{Disabled, false, false, false},
		{Removed, false, false, false},
	}
	for _, test := range tests {
		if InService(test.phase) != test.inService || KeepsStacks(test.phase) != test.keepsStacks || Up(test.phase) != test.up {
			t.Errorf("%s: in service %v, keeps stacks %v, up %v; want %v, %v, %v", test.phase,
				InService(test.phase), KeepsStacks(test.phase), Up(test.phase), test.inService, test.keepsStacks, test.up)
		}
	}
}

func TestTheAPIAsksWhetherAHostMayBeRemovedOrLeave(t *testing.T) {
	c := newCluster(t)
	c.activeHosts(1)
	c.addHost(testHost(2))
	if err := c.state.CheckRemoval(7); err == nil || err.Error() != "no host 7" {
		t.Errorf("an unknown host: %v", err)
	}
	if err := c.state.CheckLeaving(7); err == nil || err.Error() != "no host 7" {
		t.Errorf("an unknown host leaves: %v", err)
	}
	if err := c.state.CheckLeaving(2); err == nil || err.Error() != "host node2 is joining; only an active or draining host leaves" {
		t.Errorf("a joining host leaves: %v", err)
	}
	if err := c.state.CheckRemoval(1); err != nil {
		t.Errorf("an empty host: %v", err)
	}
	// a host with a stack is not removed at once, but it may leave
	c.putShop(shopSpec())
	host := HostID(1)
	c.mustApply(KindSetStackStatus, LeaderActor, SetStackStatus{PlannedAt: c.index, ID: "default/shop", Status: StackStarting, Host: &host})
	if err := c.state.CheckRemoval(1); err == nil || err.Error() != "host node1 still has stack default/shop: it must move off first" {
		t.Errorf("a host with a stack: %v", err)
	}
	if err := c.state.CheckLeaving(1); err != nil {
		t.Errorf("a host with a stack leaves: %v", err)
	}
	c.mustApply(KindSetHostPhase, cliActor, SetHostPhase{Host: 2, Phase: Removed})
	if err := c.state.CheckRemoval(2); err == nil || err.Error() != "host node2 is removed" {
		t.Errorf("a removed host: %v", err)
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

func TestASecondHostMayGoBeforeTheClusterIsHighlyAvailable(t *testing.T) {
	c := newCluster(t)
	c.addHost(testHost(1))
	c.mustApply(KindSetHostPhase, LeaderActor, SetHostPhase{Host: 1, Phase: Active, From: Joining})
	if c.state.HA {
		t.Error("two active hosts are highly available")
	}
	c.mustApply(KindSetHostPhase, cliActor, SetHostPhase{Host: 1, Phase: Removed})
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
