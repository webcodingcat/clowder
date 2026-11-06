package scheduler

import (
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/webcodingcat/clowder/internal/clusterspec"
	"github.com/webcodingcat/clowder/internal/hostrole"
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

// planTime is the leader's clock in the tests' plans.
var planTime = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

// cluster is one active host with every role, a 100 GiB fast and a 10 GiB
// bulk pool.
func cluster() *state.State {
	return &state.State{
		Config: state.ClusterConfig{Spec: clusterspec.Config{StorageClasses: map[string]clusterspec.StorageClass{
			"fast": {Pool: "vg-fast/thin"},
			"bulk": {Pool: "vg-bulk/thin"},
			"any":  {Fallback: []string{"bulk", "fast"}},
		}}},
		Hosts: map[state.HostID]*state.Host{0: {
			ID: 0, Name: "node0", Roles: hostrole.DefaultInit, Phase: state.Active, Pools: map[string]state.Pool{
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
