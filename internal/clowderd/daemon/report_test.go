package daemon

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"sync"
	"testing"

	"github.com/webcodingcat/clowder/internal/clowderd/daemon/daemontest"
	"github.com/webcodingcat/clowder/internal/clowderd/hostfiles"
	"github.com/webcodingcat/clowder/internal/state"
	"github.com/webcodingcat/clowder/internal/storage"
)

// newTestReporter reports host 0 of daemontest.NewCluster.
func newTestReporter(t *testing.T) (*hostReporter, *daemontest.Cluster, *daemontest.Docker, *daemontest.Storage) {
	t.Helper()
	cluster := daemontest.NewCluster(t)
	docker, storage := &daemontest.Docker{}, &daemontest.Storage{Dir: t.TempDir()}
	reporter := &hostReporter{
		host: hostfiles.Host{ID: 0, Name: "node0"}, version: "1.0.0", cluster: cluster, docker: docker, storage: storage,
		propose: cluster.Propose, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), root: t.TempDir(),
		seen: noCopiesSeen,
	}
	return reporter, cluster, docker, storage
}

// reported is host 0 as the state has it, and how many reports it took.
func reported(cluster *daemontest.Cluster) (state.Host, int) {
	cluster.Lock()
	defer cluster.Unlock()
	count := 0
	for _, entry := range cluster.State.Audit {
		if entry.Kind == state.KindReportHost {
			count++
		}
	}
	return *cluster.State.Hosts[0], count
}

// set changes a fake under its lock.
func set(mu sync.Locker, change func()) {
	mu.Lock()
	defer mu.Unlock()
	change()
}

func TestTheHostIsReportedOnlyWhenAValueMovesByAPercent(t *testing.T) {
	reporter, cluster, _, storage := newTestReporter(t)
	set(storage, func() { storage.DataPercent = 79.2 })
	if err := reporter.pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	host, count := reported(cluster)
	if pool := host.Pools["vg-fast/thin"]; pool.DataPercent != 80 || pool.SizeBytes != 100<<30 || host.DockerDown ||
		host.Version != "1.0.0" || count != 1 {
		t.Errorf("host %+v after %d reports", host, count)
	}
	set(storage, func() { storage.DataPercent = 79.9 }) // the same whole percent, rounded up
	if err := reporter.pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, count := reported(cluster); count != 1 {
		t.Errorf("%d reports, want one while nothing moved by a percent", count)
	}
}

func TestAHangingDockerIsReportedAndAFailedPoolKeepsItsLastUse(t *testing.T) {
	reporter, cluster, docker, storage := newTestReporter(t)
	set(storage, func() { storage.DataPercent = 42 })
	if err := reporter.pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	set(docker, func() { docker.Hangs = true })
	set(storage, func() { storage.PoolFails = true })
	if err := reporter.pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	host, count := reported(cluster)
	if !host.DockerDown || count != 2 {
		t.Errorf("Docker down %v after %d reports", host.DockerDown, count)
	}
	if pool, ok := host.Pools["vg-fast/thin"]; !ok || pool.DataPercent != 42 {
		t.Errorf("the pool that did not read: %+v, %v", pool, ok)
	}

	// Docker answers again, then the agent stops: its ping fails because
	// it stops, which says nothing about Docker
	set(docker, func() { docker.Hangs = false })
	if err := reporter.pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	set(docker, func() { docker.Hangs = true })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := reporter.pass(ctx); err != nil {
		t.Fatal(err)
	}
	if host, count := reported(cluster); host.DockerDown || count != 3 {
		t.Errorf("Docker down %v after %d reports; a stopping agent reported", host.DockerDown, count)
	}
}

func TestTheRootFileSystemsUseIsAWholePercent(t *testing.T) {
	used, err := usedPercent(t.TempDir())
	if err != nil || used < 0 || used > 100 {
		t.Errorf("used %d %%, %v", used, err)
	}
	if _, err := usedPercent("/no/such/dir"); err == nil {
		t.Error("a missing file system was read")
	}
}

// noCopiesSeen is a volume reconciler that found no copies here.
func noCopiesSeen() map[string]storage.Observed { return nil }

func TestTheReportNamesSplitBrainsAndPrimariesWithoutQuorum(t *testing.T) {
	splitBrain, noQuorum := drbdProblems(map[string]storage.Observed{
		"default/a": {Role: storage.RoleSecondary, Disk: storage.DiskUpToDate, Quorum: true, SplitBrain: true},
		"default/b": {Role: storage.RolePrimary, Disk: storage.DiskUpToDate},
		// a secondary without quorum writes nothing, so nothing hangs
		"default/c": {Role: storage.RoleSecondary, Disk: storage.DiskUpToDate},
		"default/d": {Role: storage.RolePrimary, Disk: storage.DiskUpToDate, Quorum: true},
	})
	if !slices.Equal(splitBrain, []string{"default/a"}) || !slices.Equal(noQuorum, []string{"default/b"}) {
		t.Errorf("split brain %v, no quorum %v", splitBrain, noQuorum)
	}
}
