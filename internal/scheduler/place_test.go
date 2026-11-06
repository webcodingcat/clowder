package scheduler

import (
	"strings"
	"testing"

	"github.com/webcodingcat/clowder/internal/hostrole"
	"github.com/webcodingcat/clowder/internal/resource"
	"github.com/webcodingcat/clowder/internal/state"
)

func TestAStackGoesWhereItsVolumesFit(t *testing.T) {
	current := cluster()
	stack(current, "shop", map[string]resource.VolumeSpec{"db": volume("fast", 20, 1), "cache": volume("any", 12, 0)})
	decisions := Plan(current, planTime)
	placements, waiting := decisions.Placements, decisions.Waiting
	if len(waiting) > 0 || len(placements) != 1 || !placements[0].SetsHost || placements[0].Host != 0 {
		t.Fatalf("placements %+v, waiting %+v", placements, waiting)
	}
	volumes := map[string]state.Volume{}
	for _, placed := range placements[0].Volumes {
		volumes[placed.ID()] = placed
	}
	db, cache := volumes["default/db"], volumes["default/cache"]
	if db.Minor == nil || *db.Minor != 0 || db.Copies[0].Class != "fast" || db.Status != state.VolumeOK ||
		db.Name != "db" {
		t.Errorf("db %+v", db)
	}
	// 12 GiB do not fit the 10 GiB bulk pool: any falls back to fast
	if cache.Minor != nil || cache.Copies[0].Class != "fast" {
		t.Errorf("cache %+v", cache)
	}
}

func TestStacksThatDoNotFitWaitAndTakeNothing(t *testing.T) {
	current := cluster()
	stack(current, "big", map[string]resource.VolumeSpec{"small": volume("fast", 10, 1), "huge": volume("fast", 75, 1)})
	stack(current, "next", map[string]resource.VolumeSpec{"data": volume("fast", 80, 1)})
	decisions := Plan(current, planTime)
	placements, waiting := decisions.Placements, decisions.Waiting
	// huge (first by name) fits, small after it does not
	// the reason is shown to users: sizes in units, the hosts' reasons joined
	if len(waiting) != 1 || waiting[0].Stack != "default/big" ||
		!strings.Contains(waiting[0].Reason, ": volume small (10G, class fast) fits no pool") ||
		strings.Contains(waiting[0].Reason, "[") {
		t.Errorf("waiting %+v", waiting)
	}
	// big took nothing, so next has the whole usable 80 GiB, and minor 0
	if len(placements) != 1 || placements[0].Stack != "default/next" || *placements[0].Volumes[0].Minor != 0 {
		t.Errorf("placements %+v", placements)
	}
}

func TestAPlacedStackGetsOnlyItsNewVolumes(t *testing.T) {
	current := cluster()
	host := state.HostID(0)
	minor := 0
	shop := stack(current, "shop", map[string]resource.VolumeSpec{"db": volume("fast", 20, 1), "files": volume("fast", 50, 1)})
	shop.Host = &host
	db := current.Volumes["default/db"]
	db.Minor, db.Status, db.Copies = &minor, state.VolumeOK, []state.VolumeCopy{{Host: 0, Class: "fast"}}
	decisions := Plan(current, planTime)
	placements, waiting := decisions.Placements, decisions.Waiting
	if len(waiting) > 0 || len(placements) != 1 || placements[0].SetsHost {
		t.Fatalf("placements %+v, waiting %+v", placements, waiting)
	}
	files := placements[0].Volumes
	if len(files) != 1 || files[0].ID() != "default/files" || *files[0].Minor != 1 {
		t.Errorf("new volumes %+v", files)
	}
	app := shop.Spec.Services["app"]
	app.Volumes = app.Volumes[:1] // db only
	shop.Spec.Services["app"] = app
	if placements := Plan(current, planTime).Placements; len(placements) != 0 {
		t.Errorf("a placed stack without new volumes: %+v", placements)
	}
}

func TestAStackGoesWhereItsVolumeKeepsItsData(t *testing.T) {
	current := cluster()
	current.Hosts[1] = &state.Host{
		ID: 1, Name: "node1", Roles: hostrole.DefaultInit, Phase: state.Active, Pools: current.Hosts[0].Pools,
	}
	stack(current, "shop", map[string]resource.VolumeSpec{"data": volume("fast", 1, 0)})
	data := current.Volumes["default/data"]
	data.Status, data.Copies = state.VolumeOK, []state.VolumeCopy{{Host: 1, Class: "fast"}}
	placements := Plan(current, planTime).Placements
	if len(placements) != 1 || placements[0].Host != 1 || len(placements[0].Volumes) != 0 {
		t.Errorf("placements %+v", placements)
	}
	// a draining host keeps its copy, so the stack moves to where a new,
	// empty LV takes the place of its throw-away data
	current.Hosts[1].Phase = state.Draining
	moves := Plan(current, planTime).Moves
	if len(moves) != 1 || moves[0].To != 0 || moves[0].From != nil || len(firstStep(t, current, moves[0])) != 0 {
		t.Errorf("moves %+v", moves)
	}
}

func TestAReplicatedVolumeWaitsForAFreeDRBDMinor(t *testing.T) {
	current := cluster()
	current.RetiredMinors = map[int]string{}
	for minor := range state.MaxMinor + 1 {
		current.RetiredMinors[minor] = "default/old"
	}
	stack(current, "shop", map[string]resource.VolumeSpec{"db": volume("fast", 1, 1)})
	stack(current, "cache", map[string]resource.VolumeSpec{"tmp": volume("fast", 1, 0)})
	decisions := Plan(current, planTime)
	if len(decisions.Waiting) != 1 || decisions.Waiting[0].Stack != "default/shop" ||
		!strings.Contains(decisions.Waiting[0].Reason, "no free DRBD minor") {
		t.Errorf("waiting %+v", decisions.Waiting)
	}
	// a volume of replicas 0 has no DRBD, so it needs no minor
	if len(decisions.Placements) != 1 || decisions.Placements[0].Stack != "default/cache" {
		t.Errorf("placements %+v", decisions.Placements)
	}
}

func TestAStackGoesOnlyToAWorker(t *testing.T) {
	current := cluster()
	current.Hosts[0].Roles = hostrole.Set{hostrole.ControlPlane, hostrole.Ingress}
	stack(current, "shop", map[string]resource.VolumeSpec{"db": volume("fast", 1, 0)})
	if waiting := Plan(current, planTime).Waiting; len(waiting) != 1 || waiting[0].Reason != "no active worker" {
		t.Errorf("without a worker: waiting %+v", waiting)
	}
	current.Hosts[1] = &state.Host{
		ID: 1, Name: "node1", Roles: hostrole.Set{hostrole.Worker}, Phase: state.Active, Pools: current.Hosts[0].Pools,
	}
	if placements := Plan(current, planTime).Placements; len(placements) != 1 || placements[0].Host != 1 {
		t.Errorf("placements %+v", placements)
	}
}

func TestWithoutAnActiveHostEveryStackWaits(t *testing.T) {
	current := cluster()
	current.Hosts[0].Phase = state.Draining
	stack(current, "shop", map[string]resource.VolumeSpec{"db": volume("fast", 1, 0)})
	if waiting := Plan(current, planTime).Waiting; len(waiting) != 1 || waiting[0].Reason != "no active worker" {
		t.Errorf("waiting %+v", waiting)
	}
}
