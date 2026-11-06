package state

import (
	"slices"
	"strings"
	"testing"

	"github.com/webcodingcat/clowder/internal/clusterspec"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/resource"
)

func TestAVolumeWaitsForItsStackAndCannotChangeYet(t *testing.T) {
	c := newCluster(t)
	c.configure(testConfig())
	spec := testVolumeSpec()
	unknown := spec
	unknown.Class = "slow"
	c.refuseChange(putChange(resource.KindVolume, "default", "data", unknown), `no storage class "slow"`)
	c.refuseChange(putChange(resource.KindVolume, "default", "data", resource.VolumeSpec{}), "class is required")
	c.put(resource.KindVolume, "default", "data", spec)
	volume := c.state.Volumes["default/data"]
	if volume.Status != VolumePending || volume.DataGeneration != 1 || len(volume.Copies) != 0 {
		t.Errorf("new volume %+v", volume)
	}
	larger := spec
	larger.Size *= 2
	c.refuseChange(putChange(resource.KindVolume, "default", "data", larger), "cannot change a volume yet")
	// its class stays while it is there
	config := testConfig()
	config.StorageClasses = map[string]clusterspec.StorageClass{"slow": {Pool: "vg-slow/thin"}}
	c.refuseChange(configChange(config), "volume default/data has storage class fast, so the class must stay")
	// the leader records where it is, and only that
	placed := *volume
	placed.Status = VolumeOK
	placed.Copies = []VolumeCopy{{Host: 0, Class: "fast"}}
	placed.Spec.Replicas = 0
	c.refuse(KindPutVolume, LeaderActor, PutVolume{PlannedAt: c.index, Volume: placed}, "its spec changes only with an apply")
	placed.Spec.Replicas = 1
	// the record as it is, with a Meta the sender read long ago: nothing changes
	stale := *volume
	stale.ResourceVersion = 1
	if changes, err := c.changes(KindPutVolume, LeaderActor, PutVolume{PlannedAt: c.index, Volume: stale}); err != nil || len(changes) != 0 {
		t.Errorf("a pending volume as it is: changes %v, %v", changes, err)
	}
	pendingWithCopy := *volume
	pendingWithCopy.Copies = placed.Copies
	c.refuse(KindPutVolume, LeaderActor, PutVolume{PlannedAt: c.index, Volume: pendingWithCopy}, "a pending volume has no copies")
	minor := 3
	placed.Minor = &minor
	changes, err := c.changes(KindPutVolume, LeaderActor, PutVolume{PlannedAt: c.index, Volume: placed})
	if err != nil || len(changes) != 1 || changes[0].Name != "data" {
		t.Errorf("placed: changes %v, %v", changes, err)
	}
	if changes, err := c.changes(KindPutVolume, LeaderActor, PutVolume{PlannedAt: c.index, Volume: *c.state.Volumes["default/data"]}); err != nil ||
		len(changes) != 0 {
		t.Errorf("the same record again: changes %v, %v", changes, err)
	}
	// placed, it keeps a copy: only its delete gives up its data
	backToPending := *c.state.Volumes["default/data"]
	backToPending.Status, backToPending.Copies, backToPending.Minor = VolumePending, nil, nil
	c.refuse(KindPutVolume, LeaderActor, PutVolume{PlannedAt: c.index, Volume: backToPending}, "holds data")
	noCopy := *c.state.Volumes["default/data"]
	noCopy.Copies = nil
	c.refuse(KindPutVolume, LeaderActor, PutVolume{PlannedAt: c.index, Volume: noCopy}, "holds data")
	// a volume made again after its delete starts above its retired copies
	c.del(resource.KindVolume, "default", "data")
	if want := []RetiredCopy{{Volume: "default/data", DataGeneration: 1, Host: 0}}; !slices.Equal(c.state.Retired, want) {
		t.Errorf("retired %v, want %v", c.state.Retired, want)
	}
	c.put(resource.KindVolume, "default", "data", spec)
	if generation := c.state.Volumes["default/data"].DataGeneration; generation != 2 {
		t.Errorf("data generation %d after a retired copy of 1", generation)
	}
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

func TestACopyKeepsItsSyncedWordWhileItStaysWhatItWas(t *testing.T) {
	c := moveCluster(t)
	c.refuse(KindSetCopySynced, HostActor(1), SetCopySynced{Volume: "default/data", DataGeneration: 1, Synced: true},
		"host/1 has no copy of volume default/data with a disk")
	c.refuse(KindSetCopySynced, HostActor(0), SetCopySynced{Volume: "default/data", DataGeneration: 2, Synced: true},
		"data generation 1, not 2")

	// the leader adds a copy on host 1 and cannot call it synced
	c.mustApply(KindPutVolume, LeaderActor, PutVolume{PlannedAt: c.index, Volume: c.withCopies(
		VolumeCopy{Host: 0, Class: "fast"}, VolumeCopy{Host: 1, Class: "fast", Synced: true},
	)})
	copies := c.state.Volumes["default/data"].Copies
	if !copies[0].Synced || copies[1].Synced {
		t.Errorf("the old copy keeps its word, the new one has none: %+v", copies)
	}
	c.mustApply(KindSetCopySynced, HostActor(1), SetCopySynced{Volume: "default/data", DataGeneration: 1, Synced: true})
	if !c.state.Volumes["default/data"].Copies[1].Synced {
		t.Errorf("host 1 told its copy is synced: %+v", c.state.Volumes["default/data"].Copies)
	}
	// a copy that becomes the tiebreaker keeps the class of its LV, which
	// its host removes, and is not synced
	c.refuse(KindPutVolume, LeaderActor, PutVolume{PlannedAt: c.index, Volume: c.withCopies(
		VolumeCopy{Host: 0, Class: "fast"}, VolumeCopy{Host: 1, Tiebreaker: true},
	)}, "must keep its class \"fast\"")
	c.mustApply(KindPutVolume, LeaderActor, PutVolume{PlannedAt: c.index, Volume: c.withCopies(
		VolumeCopy{Host: 0, Class: "fast"}, VolumeCopy{Host: 1, Class: "fast", Tiebreaker: true},
	)})
	if copies := c.state.Volumes["default/data"].Copies; copies[1].Synced {
		t.Errorf("a tiebreaker is synced: %+v", copies)
	}
}

func TestAVolumeHasOneCopyMoreThanItsReplicasAtMostAndATiebreakerOnlyForTwoCopies(t *testing.T) {
	c := moveCluster(t)
	c.activeHost(3)
	tests := []struct {
		name      string
		copies    []VolumeCopy
		wantError string
	}{
		{"three copies of two, while one moves", []VolumeCopy{{Host: 0}, {Host: 1}, {Host: 2}}, ""},
		{"two copies and a tiebreaker", []VolumeCopy{{Host: 0}, {Host: 1}, {Host: 2, Tiebreaker: true}}, ""},
		{"four copies of two", []VolumeCopy{{Host: 0}, {Host: 1}, {Host: 2}, {Host: 3}}, "4 copies for 2 replicas"},
		{
			"two tiebreakers",
			[]VolumeCopy{{Host: 0}, {Host: 1, Tiebreaker: true}, {Host: 2, Tiebreaker: true}},
			"2 tiebreakers for 2 replicas",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			volume := c.withCopies(test.copies...)
			for i := range volume.Copies {
				volume.Copies[i].Class = "fast"
			}
			err := checkVolume(volume)
			if test.wantError == "" && err != nil || test.wantError != "" && (err == nil || !strings.Contains(err.Error(), test.wantError)) {
				t.Errorf("got %v, want %q", err, test.wantError)
			}
		})
	}
	volume := c.withCopies(VolumeCopy{Host: 0, Class: "fast"}, VolumeCopy{Host: 1, Class: "fast", Tiebreaker: true})
	volume.Spec.Replicas = 3
	if err := checkVolume(volume); err == nil || !strings.Contains(err.Error(), "1 tiebreakers for 3 replicas") {
		t.Errorf("a tiebreaker for three copies: %v", err)
	}
	// replicas 1: a tiebreaker only while a move gives it two copies
	volume.Spec.Replicas = 1
	if err := checkVolume(volume); err == nil || !strings.Contains(err.Error(), "1 tiebreakers for 1 replicas") {
		t.Errorf("a tiebreaker for one copy: %v", err)
	}
	volume = c.withCopies(VolumeCopy{Host: 0, Class: "fast"}, VolumeCopy{Host: 1, Class: "fast"},
		VolumeCopy{Host: 2, Tiebreaker: true})
	volume.Spec.Replicas = 1
	if err := checkVolume(volume); err != nil {
		t.Errorf("a tiebreaker while replicas 1 moves: %v", err)
	}
}

func TestANewCopyWaitsUntilItsHostRemovedTheCopyItGaveUp(t *testing.T) {
	c := moveCluster(t)
	c.mustApply(KindPutVolume, LeaderActor, PutVolume{PlannedAt: c.index, Volume: c.withCopies(
		VolumeCopy{Host: 0, Class: "fast"}, VolumeCopy{Host: 1, Class: "fast"},
	)})
	c.mustApply(KindPutVolume, LeaderActor, PutVolume{PlannedAt: c.index, Volume: c.withCopies(VolumeCopy{Host: 0, Class: "fast"})})
	c.refuse(KindPutVolume, LeaderActor, PutVolume{PlannedAt: c.index, Volume: c.withCopies(
		VolumeCopy{Host: 0, Class: "fast"}, VolumeCopy{Host: 1, Class: "fast"},
	)}, "host 1 still removes a copy it gave up")
	c.mustApply(KindForgetCopy, HostActor(1), ForgetCopy{Copy: RetiredCopy{Volume: "default/data", DataGeneration: 1, Host: 1}})
	c.mustApply(KindPutVolume, LeaderActor, PutVolume{PlannedAt: c.index, Volume: c.withCopies(
		VolumeCopy{Host: 0, Class: "fast"}, VolumeCopy{Host: 1, Class: "fast"},
	)})
}
