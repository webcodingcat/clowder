package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/webcodingcat/clowder/internal/state"
)

// usedBytesStep rounds the pool use a host reports, so the report changes
// (and commits) only when the use moves by a step, not with every write.
const usedBytesStep = 1 << 30

// reportHost tells the cluster this host's version, capacity and pools when
// they differ from what the state has.
func (r *stackRunner) reportHost(ctx context.Context, desired desiredState) error {
	capacity, err := readCapacity()
	if err != nil {
		return err
	}
	report := state.ReportHost{Host: r.host.ID, Version: r.version, Capacity: capacity}
	for _, class := range desired.config.StorageClasses {
		if class.Pool == "" {
			continue
		}
		size, dataPercent, _, err := r.storage.PoolUsage(ctx, class.Pool)
		if err != nil {
			// not every host has every pool
			r.logger.Debug("read a pool", "pool", class.Pool, "error", err)
			continue
		}
		if report.Pools == nil {
			report.Pools = map[string]state.Pool{}
		}
		used := int64(float64(size)*dataPercent/100) / usedBytesStep * usedBytesStep
		report.Pools[class.Pool] = state.Pool{Name: class.Pool, SizeBytes: size, UsedBytes: used}
	}
	self := desired.self
	if report.Version == self.Version && report.Capacity == self.Capacity && maps.Equal(report.Pools, self.Pools) {
		return nil
	}
	return r.report(ctx, state.KindReportHost, report)
}

// report commits a report of this host about itself.
func (r *stackRunner) report(ctx context.Context, kind string, payload any) error {
	cmd, err := state.NewCommand(kind, time.Time{}, state.HostActor(r.host.ID), payload)
	if err != nil {
		return err
	}
	_, err = r.propose(ctx, cmd)
	return err
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
