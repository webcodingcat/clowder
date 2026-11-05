package state

import (
	"slices"
	"testing"

	"github.com/webcodingcat/clowder/internal/resource"
)

// lostMajority makes a highly available cluster of three hosts that is
// about to lose hosts 1 and 2 for good. Host 0 runs default/shop; host 1
// runs default/web. The volumes:
//   - default/db (replicas 2): synced copies on 0 and 1, kept on 0;
//   - default/logs (replicas 1): its copy on 1, a tiebreaker on 0;
//   - default/cache (replicas 2): synced on 1, not yet on 0;
//   - default/tmp (replicas 0): its LV on 2.
//
// A move of default/web to host 2 runs.
func lostMajority(t *testing.T) *cluster {
	t.Helper()
	c := newCluster(t)
	for id := HostID(1); id <= 2; id++ {
		c.addHost(testHost(id))
		c.mustApply(KindSetHostPhase, LeaderActor, SetHostPhase{Host: id, Phase: Active})
	}
	for id := range HostID(3) {
		c.state.Hosts[id].Voter = true
	}
	c.putShop(shopSpec())
	zero, one := HostID(0), HostID(1)
	c.state.Stacks["default/shop"].Host = &zero
	c.state.Stacks["default/shop"].Status = StackRunning
	c.mustApply(KindPutResource, cliActor, putResource(resource.KindStack, "default", "web", shopSpec()))
	c.state.Stacks["default/web"].Host = &one
	c.state.Stacks["default/web"].Status = StackRunning
	minor := func(n int) *int { return &n }
	volume := func(name string, replicas int, copies ...VolumeCopy) {
		spec := testVolumeSpec()
		spec.Replicas = replicas
		var deviceMinor *int
		if replicas > 0 {
			deviceMinor = minor(len(c.state.Volumes) + 1)
		}
		c.state.Volumes["default/"+name] = &Volume{
			Meta: Meta{Namespace: "default", Name: name, Owner: OwnerCLI}, Spec: spec, Status: VolumeOK, Minor: deviceMinor,
			Copies: copies, DataGeneration: 1, Initialized: true,
		}
	}
	volume("db", 2, VolumeCopy{Host: 0, Class: "fast", Synced: true}, VolumeCopy{Host: 1, Class: "fast", Synced: true})
	volume("logs", 1, VolumeCopy{Host: 1, Class: "fast", Synced: true}, VolumeCopy{Host: 0, Tiebreaker: true})
	volume("cache", 2, VolumeCopy{Host: 1, Class: "fast", Synced: true}, VolumeCopy{Host: 0, Class: "fast"})
	volume("tmp", 0, VolumeCopy{Host: 2, Class: "fast"})
	c.mustApply(KindStartOperation, LeaderActor, StartOperation{
		Kind: OperationMoveStack, Locks: []string{StackLock("default/web")}, Move: &StackMove{Stack: "default/web", From: &one, To: 2},
	})
	return c
}

func TestARecoverKeepsWhatTheSurvivorHolds(t *testing.T) {
	c := lostMajority(t)
	operation := c.index
	c.mustApply(KindRecoverRemap, LeaderActor, RecoverRemap{Survivor: 0})

	for id := HostID(1); id <= 2; id++ {
		host := c.state.Hosts[id]
		if host.Phase != Removed || host.Voter {
			t.Errorf("host %d: %s, voter %v", id, host.Phase, host.Voter)
		}
		if _, revoked := c.state.Revoked[host.CertificateSerial]; !revoked {
			t.Errorf("host %d keeps its certificate", id)
		}
		if waiting := c.state.Forgetting[id]; !slices.Equal(waiting, []HostID{0}) {
			t.Errorf("host %d is to be forgotten by %v, want the survivor", id, waiting)
		}
	}
	survivor := c.state.Hosts[0]
	if survivor.Phase != Active || !survivor.Voter || c.state.Ingress == nil || *c.state.Ingress != 0 {
		t.Errorf("the survivor: %s, voter %v, ingress %v", survivor.Phase, survivor.Voter, c.state.Ingress)
	}
	if c.state.HA || c.state.RecoveredAt != testTime {
		t.Errorf("HA %v, recovered at %v", c.state.HA, c.state.RecoveredAt)
	}

	volumes := c.state.Volumes
	if db := volumes["default/db"]; db.Status != VolumeOK || len(db.Copies) != 1 || db.Copies[0].Host != 0 {
		t.Errorf("default/db: %s %+v, want its copy on the survivor", db.Status, db.Copies)
	}
	if logs := volumes["default/logs"]; logs.Status != VolumeRestorePending || len(logs.Copies) != 0 || logs.Minor == nil {
		t.Errorf("default/logs: %s %+v; want it waiting for a restore, its minor kept", logs.Status, logs.Copies)
	}
	// the unsynced copy may hold older data: it stays, and shows
	cache := volumes["default/cache"]
	if want := []VolumeCopy{{Host: 0, Class: "fast"}}; cache.Status != VolumeRestorePending || !slices.Equal(cache.Copies, want) {
		t.Errorf("default/cache: %s %+v, want it waiting for a restore with its copy here", cache.Status, cache.Copies)
	}
	// the tiebreaker has nothing to keep
	if want := []RetiredCopy{{Volume: "default/logs", DataGeneration: 1, Host: 0}}; !slices.Equal(c.state.Retired, want) {
		t.Errorf("retired %v, want %v", c.state.Retired, want)
	}
	if tmp := volumes["default/tmp"]; tmp.Status != VolumePending || len(tmp.Copies) != 0 || tmp.DataGeneration != 2 ||
		tmp.Initialized {
		t.Errorf("default/tmp: %s %+v, generation %d; want it new, as after a failover", tmp.Status, tmp.Copies, tmp.DataGeneration)
	}

	if shop := c.state.Stacks["default/shop"]; shop.Host == nil || *shop.Host != 0 || shop.Status != StackRunning {
		t.Errorf("default/shop: %v %s, want it running where it runs", shop.Host, shop.Status)
	}
	if web := c.state.Stacks["default/web"]; web.Host != nil || web.Status != StackPending {
		t.Errorf("default/web: %v %s, want it placed anew", web.Host, web.Status)
	}
	if moving := c.state.Operations[operation]; moving.State != OpFailed || len(c.state.Locks) != 0 {
		t.Errorf("the move: %s, locks %v", moving.State, c.state.Locks)
	}

	// once more, as recover after a failure: nothing changes
	before := c.encoded()
	c.mustApply(KindRecoverRemap, LeaderActor, RecoverRemap{Survivor: 0})
	if after := c.encoded(); after != before {
		t.Errorf("a second remap changed the state:\n%s\n%s", before, after)
	}
}

func TestOnlyClowderdRecoverRemapsTheCluster(t *testing.T) {
	c := lostMajority(t)
	c.refuse(KindRecoverRemap, cliActor, RecoverRemap{Survivor: 0}, "only clowderd recover")
	c.refuse(KindRecoverRemap, HostActor(0), RecoverRemap{Survivor: 0}, "only clowderd recover")
	c.refuse(KindRecoverRemap, LeaderActor, RecoverRemap{Survivor: 7}, "no host 7")
}

func TestTheClusterIsHighlyAvailableAgainWithThreeHosts(t *testing.T) {
	c := lostMajority(t)
	c.mustApply(KindRecoverRemap, LeaderActor, RecoverRemap{Survivor: 0})
	for id := HostID(3); id <= 4; id++ {
		c.addHost(testHost(id))
	}
	c.mustApply(KindSetHostPhase, LeaderActor, SetHostPhase{Host: 3, Phase: Active})
	if c.state.HA || c.state.RecoveredAt.IsZero() {
		t.Fatal("highly available with two hosts")
	}
	c.mustApply(KindSetHostPhase, LeaderActor, SetHostPhase{Host: 4, Phase: Active})
	if !c.state.HA || !c.state.RecoveredAt.IsZero() {
		t.Errorf("HA %v, recovered at %v", c.state.HA, c.state.RecoveredAt)
	}
}

func TestAVolumeWithoutDataTakesNoNewCopy(t *testing.T) {
	c := lostMajority(t)
	c.mustApply(KindRecoverRemap, LeaderActor, RecoverRemap{Survivor: 0})
	placed := *c.state.Volumes["default/logs"]
	placed.Status, placed.Copies = VolumeOK, []VolumeCopy{{Host: 0, Class: "fast"}}
	c.refuse(KindPutVolume, LeaderActor, PutVolume{PlannedAt: c.index, Volume: placed}, "waits for a restore")
}

func TestAVolumeThatWaitsForARestoreIsDeletedWithItsCopy(t *testing.T) {
	c := lostMajority(t)
	c.mustApply(KindRecoverRemap, LeaderActor, RecoverRemap{Survivor: 0})
	// until a restore, a user may delete the volume, its copy here with it
	c.mustApply(KindDeleteResource, cliActor,
		DeleteResource{Kind: resource.KindVolume, Namespace: "default", Name: "cache", Owner: OwnerCLI})
	if _, ok := c.state.Volumes["default/cache"]; ok {
		t.Fatal("the volume is still there")
	}
	retired := RetiredCopy{Volume: "default/cache", DataGeneration: 1, Host: 0}
	if !slices.Contains(c.state.Retired, retired) || c.state.RetiredMinors[3] != "default/cache" {
		t.Errorf("retired %v, minors %v; want the survivor's copy removed and its minor kept until then",
			c.state.Retired, c.state.RetiredMinors)
	}
}
