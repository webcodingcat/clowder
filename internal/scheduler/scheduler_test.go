package scheduler

import (
	"context"
	"strings"
	"testing"

	"github.com/webcodingcat/clowder/internal/clusterdir"
	"github.com/webcodingcat/clowder/internal/state"
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

func TestAStackNeverAppliedIsNotPlaced(t *testing.T) {
	current := cluster()
	current.Stacks["shop"] = &state.Stack{Name: "shop", Status: state.StackCreated}
	if placements, waiting := Plan(context.Background(), current); len(placements)+len(waiting) > 0 {
		t.Errorf("placements %+v, waiting %+v", placements, waiting)
	}
}

func TestAStackGoesWhereItsVolumesFit(t *testing.T) {
	current := cluster()
	current.Stacks["shop"] = &state.Stack{Name: "shop", Source: source("shop",
		"  db:\n    x-clowder: {class: fast, size: 20G, replicas: 1}\n"+
			"  cache:\n    x-clowder: {class: any, size: 12G, replicas: 0}\n")}
	placements, waiting := Plan(context.Background(), current)
	if len(waiting) > 0 || len(placements) != 1 || !placements[0].Place || placements[0].Host != 0 {
		t.Fatalf("placements %+v, waiting %+v", placements, waiting)
	}
	volumes := map[string]state.Volume{}
	for _, volume := range placements[0].Volumes {
		volumes[volume.ID] = volume
	}
	db, cache := volumes["shop/db"], volumes["shop/cache"]
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
	current.Stacks["big"] = &state.Stack{Name: "big", Source: source("big",
		"  small:\n    x-clowder: {class: fast, size: 10G, replicas: 1}\n"+
			"  huge:\n    x-clowder: {class: fast, size: 75G, replicas: 1}\n")}
	current.Stacks["next"] = &state.Stack{Name: "next", Source: source("next",
		"  data:\n    x-clowder: {class: fast, size: 80G, replicas: 1}\n")}
	placements, waiting := Plan(context.Background(), current)
	// huge (first by name) fits, small after it does not
	if len(waiting) != 1 || waiting[0].Stack != "big" || !strings.Contains(waiting[0].Reason, "volume small") {
		t.Errorf("waiting %+v", waiting)
	}
	// big took nothing, so next has the whole usable 80 GiB, and minor 0
	if len(placements) != 1 || placements[0].Stack != "next" || *placements[0].Volumes[0].Minor != 0 {
		t.Errorf("placements %+v", placements)
	}
}

func TestAPlacedStackGetsOnlyItsNewVolumes(t *testing.T) {
	current := cluster()
	host := state.HostID(0)
	minor := 0
	current.Stacks["shop"] = &state.Stack{Name: "shop", Host: &host, Source: source("shop",
		"  db:\n    x-clowder: {class: fast, size: 20G, replicas: 1}\n"+
			"  files:\n    x-clowder: {class: fast, size: 50G, replicas: 1}\n")}
	current.Volumes["shop/db"] = &state.Volume{
		ID: "shop/db", SizeBytes: 20 * gib, Replicas: 1, Minor: &minor,
		Placements: []state.Placement{{Host: 0, Class: "fast"}},
	}
	placements, waiting := Plan(context.Background(), current)
	if len(waiting) > 0 || len(placements) != 1 || placements[0].Place {
		t.Fatalf("placements %+v, waiting %+v", placements, waiting)
	}
	files := placements[0].Volumes
	if len(files) != 1 || files[0].ID != "shop/files" || *files[0].Minor != 1 {
		t.Errorf("new volumes %+v", files)
	}
	current.Stacks["shop"].Source = source("shop", "  db:\n    x-clowder: {class: fast, size: 20G, replicas: 1}\n")
	if placements, _ := Plan(context.Background(), current); len(placements) != 0 {
		t.Errorf("a placed stack without new volumes: %+v", placements)
	}
}

func TestWithoutAnActiveHostEveryStackWaits(t *testing.T) {
	current := cluster()
	current.Hosts[0].Phase = state.Draining
	current.Stacks["shop"] = &state.Stack{Name: "shop", Source: source("shop", "  db:\n    x-clowder: {class: fast, size: 1G, replicas: 0}\n")}
	if _, waiting := Plan(context.Background(), current); len(waiting) != 1 || waiting[0].Reason != "no active host" {
		t.Errorf("waiting %+v", waiting)
	}
}
