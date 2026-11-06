package daemon

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/webcodingcat/clowder/internal/clowderd/daemon/daemontest"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/resource"
	"github.com/webcodingcat/clowder/internal/stackspec"
	"github.com/webcodingcat/clowder/internal/state"
)

// moveFacts are what the move's steps see of a host.
type moveFacts struct {
	imagesReady, stopped, released bool
}

func (f *moveFacts) steps(cluster *daemontest.Cluster, self state.HostID) moveSteps {
	return moveSteps{
		cluster: cluster, self: self, propose: cluster.Propose,
		imagesReady: func(string) bool { return f.imagesReady },
		stopped:     func(string) (bool, error) { return f.stopped, nil },
		released:    func(string) bool { return f.released },
		logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// movingBlog is a cluster with the stack blog on host 0, its volume data
// (replicas 1) there with data, and a move of blog to host 1 that placed
// a copy there. It returns the move's ID.
func movingBlog(t *testing.T) (*daemontest.Cluster, uint64) {
	t.Helper()
	cluster := daemontest.NewCluster(t)
	cluster.AddHost(1)
	cluster.Put(resource.KindVolume, "data", resource.VolumeSpec{Class: "fast", Size: 1 << 30, Replicas: 1})
	cluster.Put(resource.KindStack, "blog", stackspec.Spec{Services: map[string]stackspec.Service{"app": {
		Image: "nginx", Volumes: []stackspec.Mount{{Volume: "data", Path: "/data"}},
	}}})
	var volume state.Volume
	cluster.Read(func(current *state.State) { volume = *current.Volumes[names.ID(names.Default, "data")] })
	minor := 5
	volume.Minor, volume.Status = &minor, state.VolumeOK
	volume.Copies = []state.VolumeCopy{{Host: 0, Class: "fast"}}
	cluster.Apply(state.KindPutVolume, state.LeaderActor, state.PutVolume{Volume: volume, PlannedAt: cluster.Index()})
	cluster.Place("blog")
	cluster.Apply(state.KindSetVolumeInitialized, state.HostActor(0), state.SetVolumeInitialized{Volume: "default/data", DataGeneration: 1})
	zero, one := state.HostID(0), state.HostID(1)
	cluster.Apply(state.KindStartOperation, state.LeaderActor, state.StartOperation{
		Kind: state.OperationMoveStack, Locks: []string{"stack/default/blog", "volume/default/data"},
		Move: &state.StackMove{Stack: "default/blog", From: &zero, To: 1},
	})
	operation := cluster.Index()
	volume.Copies = append(volume.Copies, state.VolumeCopy{Host: 1, Class: "fast"})
	cluster.Apply(state.KindPutVolume, state.LeaderActor, state.PutVolume{
		Volume: volume, PlannedAt: cluster.Index(), Operation: operation,
	})
	cluster.Apply(state.KindAdvanceOperation, state.LeaderActor, state.AdvanceOperation{
		ID: operation, FromStep: 0, Step: state.MoveCopies, Owner: &one,
	})
	return cluster, operation
}

// stepOf is the last step done of an operation.
func stepOf(cluster *daemontest.Cluster, operation uint64) int {
	var step int
	cluster.Read(func(current *state.State) { step = current.Operations[operation].Step })
	return step
}

func TestTheNewHostWaitsForItsCopiesAndTheImages(t *testing.T) {
	cluster, operation := movingBlog(t)
	facts := &moveFacts{imagesReady: true}
	newHost := facts.steps(cluster, 1)
	newHost.pass(context.Background())
	if step := stepOf(cluster, operation); step != state.MoveCopies {
		t.Fatalf("step %d before the copy is synced", step)
	}
	cluster.Apply(state.KindSetCopySynced, state.HostActor(1), state.SetCopySynced{Volume: "default/data", DataGeneration: 1, Synced: true})
	facts.imagesReady = false
	newHost.pass(context.Background())
	if step := stepOf(cluster, operation); step != state.MoveCopies {
		t.Fatalf("step %d before the images are there", step)
	}
	facts.imagesReady = true
	facts.steps(cluster, 0).pass(context.Background()) // not the old host's step
	newHost.pass(context.Background())
	var got *state.Operation
	cluster.Read(func(current *state.State) { got = current.Operations[operation] })
	if got.Step != state.MoveSynced || *got.Owner != 0 {
		t.Fatalf("after the new host's step: %+v", got)
	}
	cluster.Read(func(current *state.State) {
		if !current.StopsForMove("default/blog") {
			t.Error("the old host does not stop the stack")
		}
	})
}

func TestTheOldHostRecordsItsStepOnceTheStackStoppedAndTheCopyIsSecondary(t *testing.T) {
	cluster, operation := movingBlog(t)
	cluster.Apply(state.KindSetCopySynced, state.HostActor(1), state.SetCopySynced{Volume: "default/data", DataGeneration: 1, Synced: true})
	(&moveFacts{imagesReady: true}).steps(cluster, 1).pass(context.Background())
	facts := &moveFacts{stopped: true}
	oldHost := facts.steps(cluster, 0)
	oldHost.pass(context.Background())
	if step := stepOf(cluster, operation); step != state.MoveSynced {
		t.Fatalf("step %d while the copy is still primary", step)
	}
	facts.stopped, facts.released = false, true
	oldHost.pass(context.Background())
	if step := stepOf(cluster, operation); step != state.MoveSynced {
		t.Fatalf("step %d while the stack runs", step)
	}
	facts.stopped = true
	oldHost.pass(context.Background())
	if step := stepOf(cluster, operation); step != state.MoveStopped {
		t.Errorf("step %d once stopped and released", step)
	}
}
