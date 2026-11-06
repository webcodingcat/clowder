package api

import (
	"context"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/webcodingcat/clowder/internal/state"
)

// fakeRecover records the rejoins and reports one synced copy.
type fakeRecover struct {
	mu       sync.Mutex
	rejoined [][]state.HostID
}

func (f *fakeRecover) DiskStates(context.Context) (map[string]string, error) {
	return map[string]string{"clw-default-db-g1": "UpToDate"}, nil
}

func (f *fakeRecover) Rejoin(_ context.Context, retired []state.HostID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rejoined = append(f.rejoined, retired)
	return nil
}

func (f *fakeRecover) rejoins() [][]state.HostID {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.rejoined)
}

func TestRecoverAsksAWorkerForItsCopiesAndHasItRejoin(t *testing.T) {
	cluster := newTestCluster(t)
	ctx := context.Background()
	caller := NewRecoverCaller(cluster.hostCertificate(0), cluster.ca.Certificate, testVersion)
	worker := cluster.endpoint(int(workerHost))

	disks, err := caller.Copies(ctx, worker)
	if want := map[string]string{"clw-default-db-g1": "UpToDate"}; err != nil || !maps.Equal(disks, want) {
		t.Errorf("copies %v, %v; want %v", disks, err, want)
	}

	// while it knows a leader, its cluster is not lost
	if err := caller.Rejoin(ctx, worker, []state.HostID{1}); err == nil || !strings.Contains(err.Error(), "knows a leader") {
		t.Errorf("a rejoin while the worker knows a leader: %v", err)
	}
	for _, id := range []state.HostID{1, workerHost} {
		cluster.clusters[id].mu.Lock()
		cluster.clusters[id].noLeader = true
		cluster.clusters[id].mu.Unlock()
	}
	if err := caller.Rejoin(ctx, worker, []state.HostID{1}); err != nil {
		t.Fatal(err)
	}
	if rejoined := cluster.recovers[workerHost].rejoins(); len(rejoined) != 1 || !slices.Equal(rejoined[0], []state.HostID{1}) {
		t.Errorf("rejoins %v, want one without host 1", rejoined)
	}

	// a control-plane host keeps its log
	err = caller.Rejoin(ctx, cluster.endpoint(1), []state.HostID{2})
	if err == nil || !strings.Contains(err.Error(), "a control-plane host does not rejoin") ||
		len(cluster.recovers[1].rejoins()) != 0 {
		t.Errorf("a rejoin of a control-plane host: %v", err)
	}
}
