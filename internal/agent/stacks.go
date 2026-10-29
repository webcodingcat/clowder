package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/webcodingcat/clowder/internal/api"
	"github.com/webcodingcat/clowder/internal/atomicfile"
	"github.com/webcodingcat/clowder/internal/clusterdir"
	"github.com/webcodingcat/clowder/internal/compose"
	"github.com/webcodingcat/clowder/internal/network"
	"github.com/webcodingcat/clowder/internal/raftnode"
	"github.com/webcodingcat/clowder/internal/state"
	"github.com/webcodingcat/clowder/internal/storage"
	"github.com/webcodingcat/clowder/internal/supervisor"
)

// Docker is what the agent needs from Docker; docker.Client is the real one.
type Docker interface {
	supervisor.Docker
	ComposeUp(ctx context.Context, stack, file string) error
	ComposeDown(ctx context.Context, stack, file string) error
	ComposePs(ctx context.Context, stack, file string) (string, error)
	ComposeLogs(ctx context.Context, stack, file string, follow bool, out io.Writer) error
	Login(ctx context.Context, registry, username, password string) error
}

// Storage is what the agent needs from the storage driver; storage.Driver is
// the real one.
type Storage interface {
	Ensure(ctx context.Context, volume storage.Volume) error
	Remove(ctx context.Context, volume storage.Volume) error
	Trim(ctx context.Context, volumes []storage.Volume) error
	PoolUsage(ctx context.Context, pool string) (sizeBytes int64, dataPercent, metadataPercent float64, err error)
	MountPoint(volume storage.Volume) string
}

// The files of the stacks on this host: per stack its rendered Compose file
// and the record of its volume copies.
func (f Files) stacksDir() string            { return filepath.Join(f.Dir, "stacks") }
func (f Files) stackDir(name string) string  { return filepath.Join(f.stacksDir(), name) }
func (f Files) stackFile(name string) string { return filepath.Join(f.stackDir(name), "compose.yaml") }

func (f Files) volumesRecord(name string) string {
	return filepath.Join(f.stackDir(name), "volumes.json")
}

func (f Files) lastTrim() string { return filepath.Join(f.Dir, "storage", "last-trim") }

const (
	// usedBytesStep rounds the pool use a host reports, so the report
	// changes (and commits) only when the use moves by a step, not with
	// every write.
	usedBytesStep = 1 << 30
	// trimInterval is how often the volumes give free blocks back.
	trimInterval = 7 * 24 * time.Hour
)

// stacks runs the stacks the state places on this host.
type stacks struct {
	files      Files
	host       Host
	version    string
	node       *raftnode.Node
	docker     Docker
	storage    Storage
	supervisor *supervisor.Supervisor
	propose    func(ctx context.Context, cmd state.Command) (uint64, error)
	open       func(sealed state.Sealed) ([]byte, error)
	logger     *slog.Logger
	// logins holds a hash of each registry's login, so the agent logs in
	// again only when it changed
	logins map[string][32]byte
}

// wanted is what this host should run, read from the state in one go.
type wanted struct {
	config     clusterdir.Config
	stacks     map[string]state.Stack
	volumes    map[string]state.Volume
	hostNames  map[state.HostID]string
	registries map[string]state.Sealed
	self       state.Host
}

func (s *stacks) read() wanted {
	w := wanted{
		stacks: map[string]state.Stack{}, volumes: map[string]state.Volume{},
		hostNames: map[state.HostID]string{}, registries: map[string]state.Sealed{},
	}
	s.node.Read(func(current *state.State) {
		w.config = current.Config.Spec
		for name, stack := range current.Stacks {
			if stack.Host != nil && *stack.Host == s.host.ID {
				w.stacks[name] = *stack
			}
		}
		for id, volume := range current.Volumes {
			w.volumes[id] = *volume
		}
		for id, host := range current.Hosts {
			w.hostNames[id] = host.Name
		}
		maps.Copy(w.registries, current.Registries)
		if self, ok := current.Hosts[s.host.ID]; ok {
			w.self = *self
		}
	})
	return w
}

// reconcile runs until ctx ends: at every change of the state and every
// reconcileInterval. Its first pass runs compose up for every stack of this
// host even if nothing changed: after a boot no container runs, as Docker
// restarts none of them itself (restart: "no").
func (s *stacks) reconcile(ctx context.Context) {
	firstPass := true
	for {
		// take the channel before reading, so no change between goes unseen
		changed := s.node.Changed()
		w := s.read()
		if err := s.reportHost(ctx, w); err != nil {
			s.logger.Error("report this host", "error", err)
		}
		s.logIn(ctx, w)
		for _, name := range slices.Sorted(maps.Keys(w.stacks)) {
			if err := s.apply(ctx, w, w.stacks[name], firstPass); err != nil {
				s.logger.Error("run a stack", "stack", name, "error", err)
			}
		}
		s.removeOthers(ctx, w)
		firstPass = false
		select {
		case <-ctx.Done():
			return
		case <-changed:
		case <-time.After(reconcileInterval):
		}
	}
}

// apply runs one stack: its volumes, its rendered Compose file, compose up
// when the file changed (or force), then the status.
func (s *stacks) apply(ctx context.Context, w wanted, stack state.Stack, force bool) error {
	parsed, err := compose.Parse(ctx, stack.Name, []byte(stack.Source))
	if err != nil {
		return err
	}
	volumes := map[string]storage.Volume{}
	for name := range parsed.Volumes {
		volume, err := s.describe(w, stack.Name+"/"+name)
		if err != nil {
			return err
		}
		volumes[name] = volume
	}
	// recorded before they exist, so a copy made by a crashed pass is still
	// found and removed later
	if err := s.record(stack.Name, slices.Collect(maps.Values(volumes))); err != nil {
		return err
	}
	mounted := map[string]compose.MountedVolume{}
	for name, volume := range volumes {
		if err := s.storage.Ensure(ctx, volume); err != nil {
			return err
		}
		mounted[name] = compose.MountedVolume{Dir: s.storage.MountPoint(volume), Generation: w.volumes[volume.ID].Generation}
	}
	floatingIP, _ := netip.ParseAddr(w.config.Ingress.FloatingIP) // clusterdir.Check let only a valid one in
	rendered, err := compose.Render(parsed, compose.Target{
		Generation: stack.Generation, FloatingIP: floatingIP, NodeAddress: s.host.NodeAddress,
		Resolver: network.Resolver, Volumes: mounted,
	})
	if err != nil {
		return err
	}
	file := s.files.stackFile(stack.Name)
	previous, _ := os.ReadFile(file) // missing for a new stack
	if err := atomicfile.Write(file, rendered, 0o600); err != nil {
		return err
	}
	if force || !bytes.Equal(previous, rendered) {
		release := s.supervisor.Hold(stack.Name)
		err := s.docker.ComposeUp(ctx, stack.Name, file)
		release()
		if err != nil {
			return err
		}
	}
	if stack.Status == state.StackRunning {
		return nil
	}
	return s.report(ctx, state.KindSetStackStatus, state.SetStackStatus{
		Name: stack.Name, Status: state.StackRunning, Host: stack.Host, Stopped: stack.Stopped,
	})
}

// describe turns a volume of the state into this host's copy of it, as the
// storage driver takes it. The LV's name carries the generation, so a
// restored volume is a new LV next to the old one; the DRBD resource has the
// LV's name.
func (s *stacks) describe(w wanted, id string) (storage.Volume, error) {
	volume, ok := w.volumes[id]
	if !ok {
		return storage.Volume{}, fmt.Errorf("volume %s is not placed yet", id)
	}
	index := slices.IndexFunc(volume.Placements, func(p state.Placement) bool { return p.Host == s.host.ID })
	if index < 0 {
		return storage.Volume{}, fmt.Errorf("volume %s has no copy on this host", id)
	}
	stackName, name, _ := strings.Cut(id, "/")
	lv := compose.VolumeName(stackName, name, volume.Generation)
	pool := w.config.StorageClasses[volume.Placements[index].Class].Pool
	described := storage.Volume{ID: id, LV: lv, Pool: pool, SizeBytes: volume.SizeBytes}
	if volume.Minor == nil {
		return described, nil // replicas 0: a plain LV
	}
	resource := &storage.Resource{Name: lv, Minor: *volume.Minor, ResyncRate: w.config.Replication.ResyncRate}
	for _, placement := range volume.Placements {
		node := storage.Node{Host: w.hostNames[placement.Host], ID: placement.Host}
		if !placement.Tiebreaker {
			group, _, _ := strings.Cut(w.config.StorageClasses[placement.Class].Pool, "/")
			node.Disk = "/dev/" + group + "/" + lv
		}
		resource.Nodes = append(resource.Nodes, node)
	}
	described.Resource = resource
	return described, nil
}

// removeOthers stops the stacks that left this host and removes the volume
// copies the state no longer places here. A stack's directory goes once
// nothing of it is left on this host.
func (s *stacks) removeOthers(ctx context.Context, w wanted) {
	entries, err := os.ReadDir(s.files.stacksDir())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		s.logger.Error("list the stacks of this host", "error", err)
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		_, staying := w.stacks[name]
		if !staying {
			if err := s.stop(ctx, name); err != nil {
				s.logger.Error("stop a stack that left this host", "stack", name, "error", err)
				continue
			}
		}
		left, err := s.removeVolumes(ctx, w, name)
		if err != nil {
			s.logger.Error("remove the volumes of a stack", "stack", name, "error", err)
			continue
		}
		if !staying && left == 0 {
			if err := os.RemoveAll(s.files.stackDir(name)); err != nil {
				s.logger.Error("forget a stack", "stack", name, "error", err)
			}
		}
	}
}

// stop runs compose down for a stack that left this host, once: its
// Compose file goes with it.
func (s *stacks) stop(ctx context.Context, name string) error {
	file := s.files.stackFile(name)
	if _, err := os.Stat(file); errors.Is(err, os.ErrNotExist) {
		return nil // stopped before
	}
	release := s.supervisor.Hold(name)
	err := s.docker.ComposeDown(ctx, name, file)
	release()
	if err != nil {
		return err
	}
	return os.Remove(file)
}

// removeVolumes removes the recorded copies of a stack's volumes that the
// state no longer places on this host in their generation, and tells how
// many copies are left.
func (s *stacks) removeVolumes(ctx context.Context, w wanted, stackName string) (int, error) {
	recorded, err := s.readRecord(stackName)
	if err != nil {
		return 0, err
	}
	var kept []storage.Volume
	var errs []error
	for _, volume := range recorded {
		if s.placedHere(w, volume) {
			kept = append(kept, volume)
			continue
		}
		if err := s.storage.Remove(ctx, volume); err != nil {
			errs = append(errs, err)
			kept = append(kept, volume)
		}
	}
	if err := s.writeRecord(stackName, kept); err != nil {
		errs = append(errs, err)
	}
	return len(kept), errors.Join(errs...)
}

// placedHere tells whether the state has a copy of the volume on this host
// in the recorded generation.
func (s *stacks) placedHere(w wanted, volume storage.Volume) bool {
	current, ok := w.volumes[volume.ID]
	if !ok || !slices.ContainsFunc(current.Placements, func(p state.Placement) bool { return p.Host == s.host.ID }) {
		return false
	}
	stackName, name, _ := strings.Cut(volume.ID, "/")
	return compose.VolumeName(stackName, name, current.Generation) == volume.LV
}

// record adds volume copies to a stack's record of the copies on this host.
func (s *stacks) record(stackName string, volumes []storage.Volume) error {
	recorded, err := s.readRecord(stackName)
	if err != nil {
		return err
	}
	for _, volume := range volumes {
		recorded = slices.DeleteFunc(recorded, func(old storage.Volume) bool { return old.LV == volume.LV })
		recorded = append(recorded, volume)
	}
	return s.writeRecord(stackName, recorded)
}

func (s *stacks) writeRecord(stackName string, volumes []storage.Volume) error {
	slices.SortFunc(volumes, func(a, b storage.Volume) int { return strings.Compare(a.LV, b.LV) })
	encoded, err := json.MarshalIndent(volumes, "", "  ")
	if err != nil {
		return fmt.Errorf("write the volumes of %s: %w", stackName, err)
	}
	return atomicfile.Write(s.files.volumesRecord(stackName), encoded, 0o600)
}

func (s *stacks) readRecord(stackName string) ([]storage.Volume, error) {
	content, err := os.ReadFile(s.files.volumesRecord(stackName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the volumes of %s: %w", stackName, err)
	}
	var volumes []storage.Volume
	if err := json.Unmarshal(content, &volumes); err != nil {
		return nil, fmt.Errorf("read the volumes of %s: %w", stackName, err)
	}
	return volumes, nil
}

// recordedVolumes are all volume copies on this host.
func (s *stacks) recordedVolumes() ([]storage.Volume, error) {
	entries, err := os.ReadDir(s.files.stacksDir())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("list the stacks of this host: %w", err)
	}
	var volumes []storage.Volume
	for _, entry := range entries {
		recorded, err := s.readRecord(entry.Name())
		if err != nil {
			return nil, err
		}
		volumes = append(volumes, recorded...)
	}
	return volumes, nil
}

// logIn logs Docker in to each private registry whose login changed.
func (s *stacks) logIn(ctx context.Context, w wanted) {
	for _, registry := range slices.Sorted(maps.Keys(w.registries)) {
		opened, err := s.open(w.registries[registry])
		if err != nil {
			s.logger.Error("open a registry login", "registry", registry, "error", err)
			continue
		}
		hash := sha256.Sum256(opened)
		if s.logins[registry] == hash {
			continue
		}
		var login clusterdir.RegistryLogin
		if err := json.Unmarshal(opened, &login); err != nil {
			s.logger.Error("read a registry login", "registry", registry, "error", err)
			continue
		}
		if err := s.docker.Login(ctx, registry, login.Username, login.Password); err != nil {
			s.logger.Error("log in to a registry", "registry", registry, "error", err)
			continue
		}
		s.logins[registry] = hash
	}
}

// reportHost tells the cluster this host's version, capacity and pools when
// they differ from what the state has.
func (s *stacks) reportHost(ctx context.Context, w wanted) error {
	capacity, err := readCapacity()
	if err != nil {
		return err
	}
	report := state.ReportHost{Host: s.host.ID, Version: s.version, Capacity: capacity}
	for _, class := range w.config.StorageClasses {
		if class.Pool == "" {
			continue
		}
		size, dataPercent, _, err := s.storage.PoolUsage(ctx, class.Pool)
		if err != nil {
			// not every host has every pool
			s.logger.Debug("read a pool", "pool", class.Pool, "error", err)
			continue
		}
		if report.Pools == nil {
			report.Pools = map[string]state.Pool{}
		}
		used := int64(float64(size)*dataPercent/100) / usedBytesStep * usedBytesStep
		report.Pools[class.Pool] = state.Pool{Name: class.Pool, SizeBytes: size, UsedBytes: used}
	}
	if report.Version == w.self.Version && report.Capacity == w.self.Capacity && maps.Equal(report.Pools, w.self.Pools) {
		return nil
	}
	return s.report(ctx, state.KindReportHost, report)
}

// report commits a report of this host about itself.
func (s *stacks) report(ctx context.Context, kind string, payload any) error {
	cmd, err := state.NewCommand(kind, time.Time{}, state.HostActor(s.host.ID), payload)
	if err != nil {
		return err
	}
	_, err = s.propose(ctx, cmd)
	return err
}

// rules tells the supervisor each service's restart rule, from the stack as
// the state has it.
func (s *stacks) rules(stackName, service string) (supervisor.Rule, bool) {
	var source string
	s.node.Read(func(current *state.State) {
		if stack, ok := current.Stacks[stackName]; ok && stack.Host != nil && *stack.Host == s.host.ID {
			source = stack.Source
		}
	})
	if source == "" {
		return supervisor.Rule{}, false
	}
	parsed, err := compose.Parse(context.Background(), stackName, []byte(source))
	if err != nil {
		return supervisor.Rule{}, false
	}
	found, ok := parsed.Services[service]
	if !ok {
		return supervisor.Rule{}, false
	}
	return supervisor.Rule{Restart: found.Restart, RestartUnhealthy: found.RestartUnhealthy}, true
}

// trim gives the free blocks of this host's volumes back to their pools
// once every trimInterval, until ctx ends. The time of the last trim is a
// file, so restarts of the agent do not put it off.
func (s *stacks) trim(ctx context.Context) {
	for {
		info, err := os.Stat(s.files.lastTrim())
		if err != nil || time.Since(info.ModTime()) >= trimInterval {
			s.trimNow(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Hour):
		}
	}
}

func (s *stacks) trimNow(ctx context.Context) {
	volumes, err := s.recordedVolumes()
	if err != nil {
		s.logger.Error("trim the volumes", "error", err)
		return
	}
	if err := s.storage.Trim(ctx, volumes); err != nil {
		s.logger.Error("trim the volumes", "error", err)
	}
	// written after a failure too: the next try is in a week, not in an hour
	if err := atomicfile.Write(s.files.lastTrim(), nil, 0o600); err != nil {
		s.logger.Error("remember the trim", "error", err)
	}
}

// Ps lists a stack's containers on this host (api.Containers).
func (s *stacks) Ps(ctx context.Context, stackName string) ([]api.ContainerInfo, error) {
	output, err := s.docker.ComposePs(ctx, stackName, s.files.stackFile(stackName))
	if err != nil {
		return nil, err
	}
	return parsePs(output)
}

// Logs writes a stack's logs on this host (api.Containers).
func (s *stacks) Logs(ctx context.Context, stackName string, follow bool, out io.Writer) error {
	return s.docker.ComposeLogs(ctx, stackName, s.files.stackFile(stackName), follow, out)
}

// parsePs reads docker compose ps --format json: one object per line.
func parsePs(output string) ([]api.ContainerInfo, error) {
	containers := []api.ContainerInfo{}
	for line := range strings.Lines(output) {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var container struct {
			Service, State, Health, Status string
		}
		if err := json.Unmarshal([]byte(line), &container); err != nil {
			return nil, fmt.Errorf("read docker compose ps: %w", err)
		}
		containers = append(containers, api.ContainerInfo{
			Service: container.Service, State: container.State, Health: container.Health, Status: container.Status,
		})
	}
	return containers, nil
}
