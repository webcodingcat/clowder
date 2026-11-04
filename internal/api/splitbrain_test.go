package api

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/webcodingcat/clowder/internal/resource"
	"github.com/webcodingcat/clowder/internal/stackspec"
	"github.com/webcodingcat/clowder/internal/state"
)

// fakeVolumes are a host's volume copies: every copy stands alone from a
// peer until it is resolved, which it records as "<volume> keep" or
// "<volume> discard".
type fakeVolumes struct {
	mu       sync.Mutex
	resolved []string
}

func (f *fakeVolumes) Progress() map[string]int64 { return nil }

func (f *fakeVolumes) ResolveSplitBrain(_ context.Context, volumeID string, discard bool) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	what := volumeID + " keep"
	if discard {
		what = volumeID + " discard"
	}
	f.resolved = append(f.resolved, what)
	return true, nil
}

func (f *fakeVolumes) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.resolved)
}

// splitVolume puts the volume shop/data with copies on node0 and node1
// into both hosts' state, and the stack shop that uses it on node0.
func splitVolume(cluster *testCluster) {
	minor := 3
	host := state.HostID(0)
	for _, fake := range cluster.clusters {
		fake.mu.Lock()
		fake.state.Volumes = map[string]*state.Volume{"shop/data": {
			Meta: state.Meta{Namespace: "shop", Name: "data"}, Spec: resource.VolumeSpec{Replicas: 2},
			Minor: &minor, Status: state.VolumeOK,
			Copies: []state.VolumeCopy{{Host: 0, Class: "fast"}, {Host: 1, Class: "fast"}},
		}}
		fake.state.Stacks = map[string]*state.Stack{"shop/shop": {
			Meta: state.Meta{Namespace: "shop", Name: "shop"}, Host: &host,
			Spec: stackspec.Spec{Services: map[string]stackspec.Service{"db": {
				Image: "postgres", Volumes: []stackspec.Mount{{Volume: "data", Path: "/data"}},
			}}},
		}}
		fake.mu.Unlock()
	}
}

func TestASplitBrainIsResolvedOnTheOtherCopiesFirstThenOnTheKeptOne(t *testing.T) {
	cluster := newTestCluster(t)
	splitVolume(cluster)
	client := cluster.client(cluster.userCertificate("alice"), testVersion, 1)
	ctx := context.Background()
	path := "/v1/volumes/shop/data/resolve-splitbrain"

	// the stack runs on node0: its copy is primary and cannot give its data up
	err := client.Act(ctx, path, ResolveRequest{Keep: "node1"}, &ResolveAnswer{})
	if statusOf(err) != http.StatusConflict || !strings.Contains(err.Error(), "keep that copy") {
		t.Errorf("want 409 for keeping the copy beside the stack's, got %v", err)
	}
	if err := client.Act(ctx, path, ResolveRequest{Keep: "node9"}, &ResolveAnswer{}); statusOf(err) != http.StatusBadRequest {
		t.Errorf("want 400 for a host without a copy, got %v", err)
	}
	var answer ResolveAnswer
	if err := client.Act(ctx, path, ResolveRequest{Keep: "node0"}, &answer); err != nil {
		t.Fatal(err)
	}
	want := []ResolvedCopy{{Host: "node1", Discarded: true, Connected: true}, {Host: "node0", Connected: true}}
	if !slices.Equal(answer.Hosts, want) {
		t.Errorf("answer %+v, want %+v", answer.Hosts, want)
	}
	if calls := cluster.volumes[1].calls(); !slices.Equal(calls, []string{"shop/data discard"}) {
		t.Errorf("node1 did %v", calls)
	}
	if calls := cluster.volumes[0].calls(); !slices.Equal(calls, []string{"shop/data keep"}) {
		t.Errorf("node0 did %v", calls)
	}
}
