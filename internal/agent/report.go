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
	"github.com/webcodingcat/clowder/internal/storage"
)

// hostReporter has a loop of its own, so that a hanging Docker cannot hold
// up the report that says so. Each probe ends at its driver's timeout
// (Docker's ping, lvs), so a pass takes at most the sum of those timeouts.
type hostReporter struct {
	host    Host
	version string
	cluster loop.Cluster
	docker  Docker
	storage Storage
	propose controller.Propose
	logger  *slog.Logger
	// root is "/" outside tests
	root string
	// seen is volumes.Reconciler.Seen
	seen func() map[string]storage.Observed
}

// run reports every loop.Interval, and at once when a new configuration may
// name a new pool. Other changes of the state do not start a probe, because
// lvs is slow on a host whose DRBD hangs.
func (r *hostReporter) run(ctx context.Context) {
	seen := int64(-1) // the configuration's generation at the last pass
	var next time.Time
	for {
		// get the channel before reading, so no change in between is missed
		changed := r.cluster.Changed()
		if generation := r.configGeneration(); generation != seen || !time.Now().Before(next) {
			seen, next = generation, time.Now().Add(loop.Interval)
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

func (r *hostReporter) configGeneration() int64 {
	var generation int64
	r.cluster.Read(func(current *state.State) { generation = current.Config.Generation })
	return generation
}

// pass keeps the state's value where a probe fails, so a pool whose lvs
// timed out is not reported as gone. The other values are still reported,
// above all whether Docker answers.
func (r *hostReporter) pass(ctx context.Context) error {
	pools, self := r.read()
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
	report.SplitBrain, report.NoQuorum = drbdProblems(r.seen())
	if report.Version == self.Version && report.Capacity == self.Capacity && maps.Equal(report.Pools, self.Pools) &&
		report.DockerDown == self.DockerDown && report.RootUsedPercent == self.RootUsedPercent &&
		slices.Equal(report.SplitBrain, self.SplitBrain) && slices.Equal(report.NoQuorum, self.NoQuorum) {
		return nil
	}
	if err := loop.CommitAsHost(ctx, r.propose, r.host.ID, state.KindReportHost, report); err != nil {
		return fmt.Errorf("commit the report: %w", err)
	}
	// log only after the state has the report, so a failed commit does not
	// log the same change again at the next pass
	switch {
	case report.DockerDown && !self.DockerDown:
		r.logger.Warn("Docker does not answer")
	case !report.DockerDown && self.DockerDown:
		r.logger.Info("Docker answers again")
	}
	for _, volume := range report.SplitBrain {
		if !slices.Contains(self.SplitBrain, volume) {
			r.logger.Error("a volume's copy stands alone from a peer: split brain", "volume", volume)
		}
	}
	return nil
}

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

// readPool rounds the use up to whole percent, like every percentage in
// the report, so a threshold trips sooner rather than later.
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

// usedPercent returns how full the file system at path is, in whole percent
// rounded up. It counts as df does: used blocks as a share of the blocks
// that a user may take.
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

func drbdProblems(seen map[string]storage.Observed) (splitBrain, noQuorum []string) {
	for _, id := range slices.Sorted(maps.Keys(seen)) {
		copyNow := seen[id]
		if copyNow.SplitBrain {
			splitBrain = append(splitBrain, id)
		}
		// a resource that is down has no device, and no quorum to report
		if copyNow.Role == storage.RolePrimary && copyNow.Disk != "" && !copyNow.Quorum {
			noQuorum = append(noQuorum, id)
		}
	}
	return splitBrain, noQuorum
}
