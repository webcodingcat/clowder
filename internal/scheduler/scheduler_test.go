package scheduler

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/webcodingcat/clowder/internal/clusterspec"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/resource"
	"github.com/webcodingcat/clowder/internal/stackspec"
	"github.com/webcodingcat/clowder/internal/state"
	"github.com/webcodingcat/clowder/internal/units"
)

const gib = int64(1) << 30

func TestVolumesFitOnlyWithoutOvercommit(t *testing.T) {
	tests := []struct {
		name      string
		allocated []int64
		size      int64
		want      bool
	}{
		{"empty pool", nil, 80 * gib, true},
		{"into the reserve", nil, 81 * gib, false},
		{"exactly full", []int64{50 * gib}, 30 * gib, true},
		{"one byte over", []int64{50 * gib}, 30*gib + 1, false},
		{"nothing", nil, 0, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := Fits(100*gib, 20, test.allocated, test.size); got != test.want {
				t.Errorf("Fits = %v", got)
			}
		})
	}
}

// cluster is one active host with a 100 GiB fast and a 10 GiB bulk pool.
func cluster() *state.State {
	return &state.State{
		Config: state.ClusterConfig{Spec: clusterspec.Config{StorageClasses: map[string]clusterspec.StorageClass{
			"fast": {Pool: "vg-fast/thin"},
			"bulk": {Pool: "vg-bulk/thin"},
			"any":  {Fallback: []string{"bulk", "fast"}},
		}}},
		Hosts: map[state.HostID]*state.Host{0: {
			ID: 0, Name: "node0", Phase: state.Active, Pools: map[string]state.Pool{
				"vg-fast/thin": {Name: "vg-fast/thin", SizeBytes: 100 * gib},
				"vg-bulk/thin": {Name: "vg-bulk/thin", SizeBytes: 10 * gib},
			},
		}},
		Stacks:  map[string]*state.Stack{},
		Volumes: map[string]*state.Volume{},
	}
}

// volume is a volume's spec: class, size in GiB, replicas.
func volume(class string, size int64, replicas int) resource.VolumeSpec {
	return resource.VolumeSpec{Class: class, Size: units.Size(size * gib), Replicas: replicas}
}

// stack adds a pending stack of the namespace default whose one service
// mounts these volumes, each a volume of the namespace that waits for its
// first stack, and returns the stack.
func stack(current *state.State, name string, volumes map[string]resource.VolumeSpec) *state.Stack {
	app := stackspec.Service{Image: "example/app"}
	for _, volumeName := range slices.Sorted(maps.Keys(volumes)) {
		app.Volumes = append(app.Volumes, stackspec.Mount{Volume: volumeName, Path: "/" + volumeName})
		current.Volumes[names.ID("default", volumeName)] = &state.Volume{
			Meta: state.Meta{Namespace: "default", Name: volumeName},
			Spec: volumes[volumeName], DataGeneration: 1, Status: state.VolumePending,
		}
	}
	added := &state.Stack{
		Meta: state.Meta{Namespace: "default", Name: name}, Status: state.StackPending,
		Spec: stackspec.Spec{Services: map[string]stackspec.Service{"app": app}},
	}
	current.Stacks[added.ID()] = added
	return added
}

func TestAStackGoesWhereItsVolumesFit(t *testing.T) {
	current := cluster()
	stack(current, "shop", map[string]resource.VolumeSpec{"db": volume("fast", 20, 1), "cache": volume("any", 12, 0)})
	decisions := Plan(current)
	placements, waiting := decisions.Placements, decisions.Waiting
	if len(waiting) > 0 || len(placements) != 1 || !placements[0].Place || placements[0].Host != 0 {
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
	decisions := Plan(current)
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
	decisions := Plan(current)
	placements, waiting := decisions.Placements, decisions.Waiting
	if len(waiting) > 0 || len(placements) != 1 || placements[0].Place {
		t.Fatalf("placements %+v, waiting %+v", placements, waiting)
	}
	files := placements[0].Volumes
	if len(files) != 1 || files[0].ID() != "default/files" || *files[0].Minor != 1 {
		t.Errorf("new volumes %+v", files)
	}
	app := shop.Spec.Services["app"]
	app.Volumes = app.Volumes[:1] // db only
	shop.Spec.Services["app"] = app
	if placements := Plan(current).Placements; len(placements) != 0 {
		t.Errorf("a placed stack without new volumes: %+v", placements)
	}
}

func TestAStackGoesWhereItsVolumeKeepsItsData(t *testing.T) {
	current := cluster()
	current.Hosts[1] = &state.Host{ID: 1, Name: "node1", Phase: state.Active, Pools: current.Hosts[0].Pools}
	stack(current, "shop", map[string]resource.VolumeSpec{"data": volume("fast", 1, 0)})
	data := current.Volumes["default/data"]
	data.Status, data.Copies = state.VolumeOK, []state.VolumeCopy{{Host: 1, Class: "fast"}}
	placements := Plan(current).Placements
	if len(placements) != 1 || placements[0].Host != 1 || len(placements[0].Volumes) != 0 {
		t.Errorf("placements %+v", placements)
	}
	current.Hosts[1].Phase = state.Draining
	if waiting := Plan(current).Waiting; len(waiting) != 1 || !strings.Contains(waiting[0].Reason, "has its data on another host") {
		t.Errorf("waiting %+v", waiting)
	}
}

func TestWithoutAnActiveHostEveryStackWaits(t *testing.T) {
	current := cluster()
	current.Hosts[0].Phase = state.Draining
	stack(current, "shop", map[string]resource.VolumeSpec{"db": volume("fast", 1, 0)})
	if waiting := Plan(current).Waiting; len(waiting) != 1 || waiting[0].Reason != "no active host" {
		t.Errorf("waiting %+v", waiting)
	}
}
