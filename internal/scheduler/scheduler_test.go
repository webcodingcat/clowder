package scheduler

import (
	"context"
	"strings"
	"testing"

	"github.com/webcodingcat/clowder/internal/clusterdir"
	"github.com/webcodingcat/clowder/internal/resource"
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
		Config: state.ClusterConfig{Spec: clusterdir.Config{StorageClasses: map[string]clusterdir.StorageClass{
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

// source is a stack's Compose file with one service and these volumes.
func source(stack, volumes string) string {
	return "name: " + stack + "\nservices:\n  app:\n    image: example/app\nvolumes:\n" + volumes
}

// stack adds a stack of the namespace default, with a Compose file with
// these volumes, and returns it.
func stack(current *state.State, name, volumes string) *state.Stack {
	added := &state.Stack{Namespace: "default", Name: name, Source: source(name, volumes)}
	current.Stacks[added.ID()] = added
	return added
}

func TestAStackNeverAppliedIsNotPlaced(t *testing.T) {
	current := cluster()
	stack(current, "shop", "").Status = state.StackCreated
	if decisions := Plan(context.Background(), current); len(decisions.Placements)+len(decisions.Waiting) > 0 {
		t.Errorf("decisions %+v", decisions)
	}
}

func TestAStackGoesWhereItsVolumesFit(t *testing.T) {
	current := cluster()
	stack(current, "shop", "  db:\n    x-clowder: {class: fast, size: 20G, replicas: 1}\n"+
		"  cache:\n    x-clowder: {class: any, size: 12G, replicas: 0}\n")
	decisions := Plan(context.Background(), current)
	placements, waiting := decisions.Placements, decisions.Waiting
	if len(waiting) > 0 || len(placements) != 1 || !placements[0].Place || placements[0].Host != 0 {
		t.Fatalf("placements %+v, waiting %+v", placements, waiting)
	}
	volumes := map[string]state.Volume{}
	for _, volume := range placements[0].Volumes {
		volumes[volume.ID] = volume
	}
	db, cache := volumes["default/shop/db"], volumes["default/shop/cache"]
	if db.Minor == nil || *db.Minor != 0 || db.Placements[0].Class != "fast" {
		t.Errorf("db %+v", db)
	}
	// 12 GiB do not fit the 10 GiB bulk pool: any falls back to fast
	if cache.Minor != nil || cache.Placements[0].Class != "fast" {
		t.Errorf("cache %+v", cache)
	}
}

func TestStacksThatDoNotFitWaitAndTakeNothing(t *testing.T) {
	current := cluster()
	stack(current, "big", "  small:\n    x-clowder: {class: fast, size: 10G, replicas: 1}\n"+
		"  huge:\n    x-clowder: {class: fast, size: 75G, replicas: 1}\n")
	stack(current, "next", "  data:\n    x-clowder: {class: fast, size: 80G, replicas: 1}\n")
	decisions := Plan(context.Background(), current)
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
	shop := stack(current, "shop", "  db:\n    x-clowder: {class: fast, size: 20G, replicas: 1}\n"+
		"  files:\n    x-clowder: {class: fast, size: 50G, replicas: 1}\n")
	shop.Host = &host
	current.Volumes["default/shop/db"] = &state.Volume{
		ID: "default/shop/db", Spec: resource.VolumeSpec{Class: "fast", Size: units.Size(20 * gib), Replicas: 1}, Minor: &minor,
		Placements: []state.Placement{{Host: 0, Class: "fast"}},
	}
	decisions := Plan(context.Background(), current)
	placements, waiting := decisions.Placements, decisions.Waiting
	if len(waiting) > 0 || len(placements) != 1 || placements[0].Place {
		t.Fatalf("placements %+v, waiting %+v", placements, waiting)
	}
	files := placements[0].Volumes
	if len(files) != 1 || files[0].ID != "default/shop/files" || *files[0].Minor != 1 {
		t.Errorf("new volumes %+v", files)
	}
	shop.Source = source("shop", "  db:\n    x-clowder: {class: fast, size: 20G, replicas: 1}\n")
	if placements := Plan(context.Background(), current).Placements; len(placements) != 0 {
		t.Errorf("a placed stack without new volumes: %+v", placements)
	}
}

func TestWithoutAnActiveHostEveryStackWaits(t *testing.T) {
	current := cluster()
	current.Hosts[0].Phase = state.Draining
	stack(current, "shop", "  db:\n    x-clowder: {class: fast, size: 1G, replicas: 0}\n")
	if waiting := Plan(context.Background(), current).Waiting; len(waiting) != 1 || waiting[0].Reason != "no active host" {
		t.Errorf("waiting %+v", waiting)
	}
}
