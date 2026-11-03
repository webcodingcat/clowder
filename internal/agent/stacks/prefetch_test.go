package stacks

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"testing"

	"github.com/webcodingcat/clowder/internal/agent/agenttest"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/resource"
	"github.com/webcodingcat/clowder/internal/stackspec"
	"github.com/webcodingcat/clowder/internal/state"
)

// blogWithCopyOn is the cluster of agenttest with host 1, and the stack
// blog (image ghost) on host 0 whose volume data (replicas 1) has its copy
// on the given host.
func blogWithCopyOn(t *testing.T, host state.HostID) *agenttest.Cluster {
	t.Helper()
	cluster := agenttest.NewCluster(t)
	cluster.AddHost(1)
	cluster.Put(resource.KindVolume, "data", resource.VolumeSpec{Class: "fast", Size: 1 << 30, Replicas: 1})
	cluster.Put(resource.KindStack, "blog", stackspec.Spec{Services: map[string]stackspec.Service{"app": {
		Image: "ghost", Volumes: []stackspec.Mount{{Volume: "data", Path: "/data"}},
	}}})
	var volume state.Volume
	cluster.Read(func(current *state.State) { volume = *current.Volumes[names.ID(names.Default, "data")] })
	minor := 5
	volume.Minor, volume.Status, volume.Copies = &minor, state.VolumeOK, []state.VolumeCopy{{Host: host, Class: "fast"}}
	cluster.Apply(state.KindPutVolume, state.LeaderActor, state.PutVolume{Volume: volume, PlannedAt: cluster.Index()})
	return cluster
}

func TestAHostPullsTheImagesOfTheStacksItMayTake(t *testing.T) {
	// blog's data has its copy on host 1, so host 1 may take blog; shop
	// (nginx) has a throw-away volume and runs on host 0, so any host may
	cluster := blogWithCopyOn(t, 1)
	docker := &agenttest.Docker{}
	prefetcher := NewPrefetcher(1, cluster, docker, slog.New(slog.NewTextHandler(io.Discard, nil)))
	prefetcher.pass(context.Background())
	if want := []string{"pull missing ghost", "pull missing nginx"}; !slices.Equal(docker.Lines(), want) {
		t.Errorf("pulled %q, want %q", docker.Lines(), want)
	}
	if !prefetcher.Ready("default/blog") {
		t.Error("blog's images are not ready")
	}
	// each pass asks again, so an image removed by hand comes back; Docker
	// pulls only what is missing
	prefetcher.pass(context.Background())
	if len(docker.Lines()) != 4 || !prefetcher.Ready("default/blog") {
		t.Errorf("the second pass: %q", docker.Lines())
	}
}

func TestAHostWithoutTheDataPullsNothingForTheStack(t *testing.T) {
	cluster := blogWithCopyOn(t, 0)
	docker := &agenttest.Docker{}
	prefetcher := NewPrefetcher(1, cluster, docker, slog.New(slog.NewTextHandler(io.Discard, nil)))
	prefetcher.pass(context.Background())
	if slices.Contains(docker.Lines(), "pull missing ghost") || prefetcher.Ready("default/blog") {
		t.Errorf("pulled %q for a stack host 1 cannot take", docker.Lines())
	}
}
