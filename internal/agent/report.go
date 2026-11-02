package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/webcodingcat/clowder/internal/agent/loop"
	"github.com/webcodingcat/clowder/internal/controller"
	"github.com/webcodingcat/clowder/internal/state"
)

// hostReporter tells the cluster about this host: its version, memory and
// CPUs, its pools, whether Docker answers and how full the root file system
// is. It has a loop of its own, so a Docker that hangs cannot hold up the
// report that says so. Each probe ends by its driver's timeout (Docker's
// ping, lvs), so a pass takes at most those timeouts added up.
type hostReporter struct {
	host    Host
	version string
	cluster loop.Cluster
	docker  Docker
	storage Storage
	propose controller.Propose
	logger  *slog.Logger
	// root is the file system whose use it reports: "/"
	root string
}

// run reports until ctx ends: every loop.Interval, and at once when a
// new cluster.yaml may name a new pool. Other changes of the state (a
// stack's status, a certificate) wake no probe: lvs on a host whose DRBD
// hangs is slow.
func (r *hostReporter) run(ctx context.Context) {
	seen := int64(-1) // the cluster.yaml revision of the last pass
	var next time.Time
	for {
		// take the channel before reading, so no change between goes unseen
		changed := r.cluster.Changed()
		if revision := r.configRevision(); revision != seen || !time.Now().Before(next) {
			seen, next = revision, time.Now().Add(loop.Interval)
			if err := r.pass(ctx); err != nil {
				r.logger.Error("report this host", "error", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-changed:
		case <-time.After(time.Until(next)):
		}
	}
}

// configRevision is the revision of the cluster.yaml the state has.
func (r *hostReporter) configRevision() int64 {
	var revision int64
	r.cluster.Read(func(current *state.State) { revision = current.Config.Revision })
	return revision
}

// pass probes the host and reports it when anything differs from what the
// state has. When a probe fails, the report keeps the state's value: a pool
// whose lvs timed out is not reported as gone. The other values are still
// reported, above all whether Docker answers.
func (r *hostReporter) pass(ctx context.Context) error {
	pools, self := r.read()
	// the state's values stand in for the probes below that fail
	report := state.ReportHost{
		Host: r.host.ID, Version: r.version, Capacity: self.Capacity, RootUsedPercent: self.RootUsedPercent,
		DockerDown: r.docker.Ping(ctx) != nil,
	}
	if ctx.Err() != nil {
		return nil // stopping: the failed ping says nothing about Docker
	}
	if capacity, err := readCapacity(); err != nil {
		r.logger.Error("report this host", "error", err)
	} else {
		report.Capacity = capacity
	}
	if rootUsed, err := usedPercent(r.root); err != nil {
		r.logger.Error("report this host", "error", err)
	} else {
		report.RootUsedPercent = rootUsed
	}
	for _, name := range pools {
		pool, err := r.readPool(ctx, name)
		if err != nil {
			last, known := self.Pools[name]
			if !known {
				// not every host has every pool
				r.logger.Debug("report this host: no pool", "pool", name, "error", err)
				continue
			}
			r.logger.Warn("report this host: the pool's last use stays", "pool", name, "error", err)
			pool = last
		}
		if report.Pools == nil {
			report.Pools = map[string]state.Pool{}
		}
		report.Pools[name] = pool
	}
	if report.Version == self.Version && report.Capacity == self.Capacity && maps.Equal(report.Pools, self.Pools) &&
		report.DockerDown == self.DockerDown && report.RootUsedPercent == self.RootUsedPercent {
		return nil
	}
	if err := loop.CommitAsHost(ctx, r.propose, r.host.ID, state.KindReportHost, report); err != nil {
		return fmt.Errorf("commit the report: %w", err)
	}
	// once the state has it, so a failed commit does not log it again
	switch {
	case report.DockerDown && !self.DockerDown:
		r.logger.Warn("Docker does not answer")
	case !report.DockerDown && self.DockerDown:
		r.logger.Info("Docker answers again")
	}
	return nil
}

// read takes from the state only what the report needs: the pools the
// storage classes name and this host as the state has it.
func (r *hostReporter) read() (pools []string, self state.Host) {
	r.cluster.Read(func(current *state.State) {
		for _, class := range current.Config.Spec.StorageClasses {
			if class.Pool != "" && !slices.Contains(pools, class.Pool) {
				pools = append(pools, class.Pool)
			}
		}
		if host, ok := current.Hosts[r.host.ID]; ok {
			self = *host
		}
	})
	slices.Sort(pools)
	return pools, self
}

// readPool reads a pool's size and use in whole percent, rounded up like
// every percent of the report, so a threshold trips sooner rather than
// later.
func (r *hostReporter) readPool(ctx context.Context, name string) (state.Pool, error) {
	size, dataPercent, metadataPercent, err := r.storage.PoolUsage(ctx, name)
	if err != nil {
		return state.Pool{}, err
	}
	return state.Pool{
		Name: name, SizeBytes: size,
		DataPercent: int(math.Ceil(dataPercent)), MetadataPercent: int(math.Ceil(metadataPercent)),
	}, nil
}

// usedPercent is how full the file system at path is, in whole percent
// rounded up, as df counts it: of the blocks a user may take, the share
// taken.
func usedPercent(path string) (int, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, fmt.Errorf("read the use of %s: %w", path, err)
	}
	used := stat.Blocks - stat.Bfree
	total := used + stat.Bavail
	if total == 0 {
		return 0, nil
	}
	return int((used*100 + total - 1) / total), nil
}

// readCapacity reads what this host has for containers: its memory
// (MemTotal in /proc/meminfo) and its CPUs.
func readCapacity() (state.Resources, error) {
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		return state.Resources{}, fmt.Errorf("read the memory of this host: %w", err)
	}
	defer func() { _ = file.Close() }() // read only
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		// "MemTotal:       16303244 kB"
		fields := strings.Fields(scanner.Text())
		if len(fields) != 3 || fields[0] != "MemTotal:" || fields[2] != "kB" {
			continue
		}
		kibibytes, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return state.Resources{}, fmt.Errorf("read the memory of this host: %w", err)
		}
		return state.Resources{MemoryBytes: kibibytes * 1024, CPUs: float64(runtime.NumCPU())}, nil
	}
	if err := scanner.Err(); err != nil {
		return state.Resources{}, fmt.Errorf("read the memory of this host: %w", err)
	}
	return state.Resources{}, errors.New("read the memory of this host: no MemTotal in /proc/meminfo")
}
