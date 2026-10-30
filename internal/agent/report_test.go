package agent

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/webcodingcat/clowder/internal/state"
)

// newTestReporter reports host 0 of newTestRunner's cluster.
func newTestReporter(t *testing.T) (*hostReporter, *testCluster, *fakeDocker, *fakeStorage) {
	t.Helper()
	runner, cluster, docker, storage := newTestRunner(t)
	reporter := &hostReporter{
		host: runner.host, version: "1.0.0", cluster: cluster, docker: docker, storage: storage,
		propose: runner.propose, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), root: t.TempDir(),
	}
	return reporter, cluster, docker, storage
}

// reported is host 0 as the state has it, and how many reports it took.
func reported(cluster *testCluster) (state.Host, int) {
	cluster.mu.Lock()
	defer cluster.mu.Unlock()
	count := 0
	for _, entry := range cluster.state.Audit {
		if entry.Kind == state.KindReportHost {
			count++
		}
	}
	return *cluster.state.Hosts[0], count
}

// set changes a fake under its lock.
func set(mu sync.Locker, change func()) {
	mu.Lock()
	defer mu.Unlock()
	change()
}

func TestTheHostIsReportedOnlyWhenAValueMovesByAPercent(t *testing.T) {
	reporter, cluster, _, storage := newTestReporter(t)
	set(&storage.mu, func() { storage.dataPercent = 79.2 })
	if err := reporter.pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	host, count := reported(cluster)
	if pool := host.Pools["vg-fast/thin"]; pool.DataPercent != 80 || pool.SizeBytes != 100<<30 || host.DockerDown ||
		host.Version != "1.0.0" || count != 1 {
		t.Errorf("host %+v after %d reports", host, count)
	}
	set(&storage.mu, func() { storage.dataPercent = 79.9 }) // the same whole percent, rounded up
	if err := reporter.pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, count := reported(cluster); count != 1 {
		t.Errorf("%d reports, want one while nothing moved by a percent", count)
	}
}

func TestAHangingDockerIsReportedAndAFailedPoolKeepsItsLastUse(t *testing.T) {
	reporter, cluster, docker, storage := newTestReporter(t)
	set(&storage.mu, func() { storage.dataPercent = 42 })
	if err := reporter.pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	set(&docker.mu, func() { docker.hangs = true })
	set(&storage.mu, func() { storage.poolFails = true })
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
	set(&docker.mu, func() { docker.hangs = false })
	if err := reporter.pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	set(&docker.mu, func() { docker.hangs = true })
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
