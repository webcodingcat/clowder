package state

import (
	"slices"
	"testing"

	"github.com/webcodingcat/clowder/internal/hostrole"
	"github.com/webcodingcat/clowder/internal/resource"
	"github.com/webcodingcat/clowder/internal/stackspec"
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
	c.put(resource.KindStack, "default", "web", shopSpec())
	c.state.Stacks["default/web"].Host = &one
	c.state.Stacks["default/web"].Status = StackRunning
	c.volume("db", 2, VolumeCopy{Host: 0, Class: "fast", Synced: true}, VolumeCopy{Host: 1, Class: "fast", Synced: true})
	c.volume("logs", 1, VolumeCopy{Host: 1, Class: "fast", Synced: true}, VolumeCopy{Host: 0, Tiebreaker: true})
	c.volume("cache", 2, VolumeCopy{Host: 1, Class: "fast", Synced: true}, VolumeCopy{Host: 0, Class: "fast"})
	c.volume("tmp", 0, VolumeCopy{Host: 2, Class: "fast"})
	c.mustApply(KindStartOperation, LeaderActor, StartOperation{
		Kind: OperationMoveStack, Locks: []string{StackLock("default/web")}, Move: &StackMove{Stack: "default/web", From: &one, To: 2},
	})
	return c
}

// volume adds a volume of the default namespace, in data generation 1,
// with the given copies.
func (c *cluster) volume(name string, replicas int, copies ...VolumeCopy) {
	spec := testVolumeSpec()
	spec.Replicas = replicas
	var minor *int
	if replicas > 0 {
		next := len(c.state.Volumes) + 1
		minor = &next
	}
	c.state.Volumes["default/"+name] = &Volume{
		Meta: Meta{Namespace: "default", Name: name, Owner: OwnerCLI}, Spec: spec, Status: VolumeOK, Minor: minor,
		Copies: copies, DataGeneration: 1, Initialized: true,
	}
}

// retiringBoth retires hosts 1 and 2 of lostMajority. DRBD says that
// default/db is synced on the survivor, and default/cache is not.
var retiringBoth = RecoverRemap{Survivor: 0, Retired: []HostID{1, 2}, Synced: []SyncedCopy{
	{Volume: "default/db", DataGeneration: 1, Host: 0},
}}

func TestARecoverKeepsWhatTheSurvivorHolds(t *testing.T) {
	c := lostMajority(t)
	operation := c.index
	c.mustApply(KindRecoverRemap, LeaderActor, retiringBoth)

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

	// once more, as recover after a failure: nothing is left to retire,
	// and nothing changes
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

// TestARecoverKeepsTheWorkersThatAnswer retires host 1 and keeps worker 2
// with its copies and stacks.
func TestARecoverKeepsTheWorkersThatAnswer(t *testing.T) {
	c := lostMajority(t)
	two := HostID(2)
	worker := c.state.Hosts[2]
	worker.Roles, worker.Voter, worker.Phase = hostrole.DefaultJoin, false, Fencing
	c.state.Ingress = &two
	// files: reported synced on the worker; media: not reported; old:
	// reported in another data generation
	c.volume("files", 2, VolumeCopy{Host: 2, Class: "fast"}, VolumeCopy{Host: 1, Class: "fast", Synced: true},
		VolumeCopy{Host: 0, Tiebreaker: true})
	c.volume("media", 1, VolumeCopy{Host: 2, Class: "fast", Synced: true}, VolumeCopy{Host: 1, Tiebreaker: true})
	c.volume("old", 1, VolumeCopy{Host: 2, Class: "fast", Synced: true}, VolumeCopy{Host: 0, Tiebreaker: true})
	for _, name := range []string{"blog", "news"} {
		c.put(resource.KindStack, "default", name, shopSpec())
		stack := c.state.Stacks["default/"+name]
		stack.Host, stack.Status = &two, StackRunning
	}
	c.state.Stacks["default/blog"].Spec = stackspec.Spec{Services: map[string]stackspec.Service{
		"app": {Image: "x", Volumes: []stackspec.Mount{{Volume: "files", Path: "/f"}}},
	}}
	c.state.Stacks["default/news"].Spec = stackspec.Spec{Services: map[string]stackspec.Service{
		"app": {Image: "x", Volumes: []stackspec.Mount{{Volume: "media", Path: "/m"}}},
	}}

	c.mustApply(KindRecoverRemap, LeaderActor, RecoverRemap{Survivor: 0, Retired: []HostID{1}, Synced: []SyncedCopy{
		{Volume: "default/db", DataGeneration: 1, Host: 0},
		{Volume: "default/files", DataGeneration: 1, Host: 2},
		{Volume: "default/old", DataGeneration: 2, Host: 2},
	}})

	if worker := c.state.Hosts[2]; worker.Phase != Active || worker.Voter {
		t.Errorf("the worker: %s, voter %v; want it active, its fence ended, without a vote", worker.Phase, worker.Voter)
	}
	if c.state.Ingress == nil || *c.state.Ingress != 2 {
		t.Errorf("the ingress is %v, want the worker that holds it", c.state.Ingress)
	}
	volumes := c.state.Volumes
	files := volumes["default/files"]
	if want := []VolumeCopy{{Host: 2, Class: "fast", Synced: true}, {Host: 0, Tiebreaker: true}}; files.Status != VolumeOK ||
		!slices.Equal(files.Copies, want) {
		t.Errorf("default/files: %s %+v, want %+v", files.Status, files.Copies, want)
	}
	for _, name := range []string{"default/media", "default/old"} {
		volume := volumes[name]
		if want := []VolumeCopy{{Host: 2, Class: "fast"}}; volume.Status != VolumeRestorePending ||
			!slices.Equal(volume.Copies, want) {
			t.Errorf("%s: %s %+v, want it waiting for a restore with the worker's copy kept", name, volume.Status,
				volume.Copies)
		}
	}
	if tmp := volumes["default/tmp"]; tmp.Status != VolumeOK || tmp.DataGeneration != 1 || len(tmp.Copies) != 1 {
		t.Errorf("default/tmp: %s %+v, generation %d; want its LV on the worker kept", tmp.Status, tmp.Copies,
			tmp.DataGeneration)
	}
	if want := []RetiredCopy{
		{Volume: "default/logs", DataGeneration: 1, Host: 0}, {Volume: "default/old", DataGeneration: 1, Host: 0},
	}; !slices.Equal(c.state.Retired, want) {
		t.Errorf("retired %v, want %v", c.state.Retired, want)
	}
	if blog := c.state.Stacks["default/blog"]; blog.Host == nil || *blog.Host != 2 || blog.Status != StackRunning {
		t.Errorf("default/blog: %v %s, want it running on the worker", blog.Host, blog.Status)
	}
	if news := c.state.Stacks["default/news"]; news.Host != nil {
		t.Errorf("default/news is on %d, want it placed anew: its volume waits for a restore", *news.Host)
	}
}

func TestARecoverCountsACopyByDRBDNotByTheStatesFlag(t *testing.T) {
	c := lostMajority(t)
	// the state says db is synced on the survivor, but DRBD did not
	c.mustApply(KindRecoverRemap, LeaderActor, RecoverRemap{Survivor: 0, Retired: []HostID{1, 2}})
	db := c.state.Volumes["default/db"]
	if want := []VolumeCopy{{Host: 0, Class: "fast"}}; db.Status != VolumeRestorePending || !slices.Equal(db.Copies, want) {
		t.Errorf("default/db: %s %+v, want it waiting for a restore with its unsynced copy kept", db.Status, db.Copies)
	}
}

func TestARecoverRetiresEveryOtherControlPlaneHost(t *testing.T) {
	c := lostMajority(t)
	c.refuse(KindRecoverRemap, LeaderActor, RecoverRemap{Survivor: 0, Retired: []HostID{1}},
		"host node2 is a control-plane host and is not retired")
	c.state.Hosts[2].Roles = hostrole.DefaultJoin
	c.refuse(KindRecoverRemap, LeaderActor, RecoverRemap{Survivor: 0, Retired: []HostID{1}, Synced: []SyncedCopy{
		{Volume: "default/db", DataGeneration: 1, Host: 1},
	}}, "host 1 does not stay")
	c.refuse(KindRecoverRemap, LeaderActor, RecoverRemap{Survivor: 0, Retired: []HostID{0, 1}},
		"host node0 survives the recover")
}

func TestOnlyAControlPlaneHostSurvivesARecover(t *testing.T) {
	c := lostMajority(t)
	c.state.Hosts[1].Roles = hostrole.DefaultJoin
	c.refuse(KindRecoverRemap, LeaderActor, RecoverRemap{Survivor: 1},
		"host node1 has the roles worker,ingress; only a control-plane host recovers")
}

func TestASurvivorWithoutIngressHoldsNoFloatingIP(t *testing.T) {
	c := lostMajority(t)
	c.state.Hosts[0].Roles = hostrole.Set{hostrole.ControlPlane, hostrole.Worker}
	one := HostID(1)
	c.state.Ingress = &one
	c.mustApply(KindRecoverRemap, LeaderActor, retiringBoth)
	if c.state.Ingress != nil {
		t.Errorf("the ingress is host %d, which has no ingress role", *c.state.Ingress)
	}
}

func TestTheClusterIsHighlyAvailableAgainWithThreeHosts(t *testing.T) {
	c := lostMajority(t)
	c.mustApply(KindRecoverRemap, LeaderActor, retiringBoth)
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
	c.mustApply(KindRecoverRemap, LeaderActor, retiringBoth)
	placed := *c.state.Volumes["default/logs"]
	placed.Status, placed.Copies = VolumeOK, []VolumeCopy{{Host: 0, Class: "fast"}}
	c.refuse(KindPutVolume, LeaderActor, PutVolume{PlannedAt: c.index, Volume: placed}, "waits for a restore")
}

func TestAVolumeThatWaitsForARestoreIsDeletedWithItsCopy(t *testing.T) {
	c := lostMajority(t)
	c.mustApply(KindRecoverRemap, LeaderActor, retiringBoth)
	// until a restore, a user may delete the volume, its copy here with it
	c.del(resource.KindVolume, "default", "cache")
	if _, ok := c.state.Volumes["default/cache"]; ok {
		t.Fatal("the volume is still there")
	}
	retired := RetiredCopy{Volume: "default/cache", DataGeneration: 1, Host: 0}
	if !slices.Contains(c.state.Retired, retired) || c.state.RetiredMinors[3] != "default/cache" {
		t.Errorf("retired %v, minors %v; want the survivor's copy removed and its minor kept until then",
			c.state.Retired, c.state.RetiredMinors)
	}
}
