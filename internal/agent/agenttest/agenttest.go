// Package agenttest has the fakes that the tests of the agent, its stack
// runner and its volume reconciler share: a cluster that applies what is
// proposed, Docker, the storage driver and the volume reconciler. Each fake is a sync.Locker: a test changes its fields
// under its lock while a loop runs.
package agenttest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/webcodingcat/clowder/internal/clusterspec"
	"github.com/webcodingcat/clowder/internal/datakey"
	"github.com/webcodingcat/clowder/internal/docker"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/resource"
	"github.com/webcodingcat/clowder/internal/stackspec"
	"github.com/webcodingcat/clowder/internal/state"
	"github.com/webcodingcat/clowder/internal/state/statetest"
	"github.com/webcodingcat/clowder/internal/storage"
)

// ShopProject is the Compose project of default/shop, the stack of
// NewCluster, and the name of all the runner keeps of it.
const ShopProject = "default_shop"

// DBLV is the LV of the volume default/db in a data generation.
func DBLV(generation int64) string {
	return names.VolumeName(names.ID(names.Default, "db"), generation)
}

// UsingDB is a stack whose service app mounts the volume db.
func UsingDB(app stackspec.Service) stackspec.Spec {
	app.Image = "nginx"
	app.Volumes = []stackspec.Mount{{Volume: "db", Path: "/data"}}
	return stackspec.Spec{Services: map[string]stackspec.Service{"app": app}}
}

// Calls is what a fake was asked to do, one line per call.
type Calls struct {
	sync.Mutex
	lines []string
}

// Add records a call.
func (c *Calls) Add(format string, args ...any) {
	c.Lock()
	defer c.Unlock()
	c.lines = append(c.lines, fmt.Sprintf(format, args...))
}

// Lines are the calls so far.
func (c *Calls) Lines() []string {
	c.Lock()
	defer c.Unlock()
	return slices.Clone(c.lines)
}

// Count is how often a call was made.
func (c *Calls) Count(line string) int {
	c.Lock()
	defer c.Unlock()
	count := 0
	for _, each := range c.lines {
		if each == line {
			count++
		}
	}
	return count
}

// WaitFor waits up to 10 s for a call.
func (c *Calls) WaitFor(t *testing.T, line string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if c.Count(line) > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("no call %q; the calls: %q", line, c.Lines())
}

// Storage stands in for the storage driver: it keeps what the host has of
// each copy (Copies) as the driver's steps would leave it, and records each
// step as "<step> <LV>". Every pool has 100 GiB, DataPercent full, unless
// reading it fails (PoolFails). A trim fails while TrimFails is set.
type Storage struct {
	Calls
	Dir         string
	DataPercent float64
	PoolFails   bool
	TrimFails   bool
	// Copies are what the host has of each copy, by LV
	Copies map[string]storage.Observed
	// Fails makes every step on an LV fail with its error
	Fails map[string]error
	// Hold makes the steps on an LV wait until its channel closes, as a
	// write to a suspended volume hangs
	Hold map[string]chan struct{}
}

// step records a step on a copy, waits while Hold says so, fails while
// Fails says so, and otherwise changes the copy.
func (f *Storage) step(ctx context.Context, name string, volume storage.Volume, change func(*storage.Observed)) error {
	f.Add("%s %s", name, volume.LV)
	f.Lock()
	hold, failure := f.Hold[volume.LV], f.Fails[volume.LV]
	f.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if failure != nil {
		return failure
	}
	f.Lock()
	defer f.Unlock()
	if f.Copies == nil {
		f.Copies = map[string]storage.Observed{}
	}
	copyNow := f.Copies[volume.LV]
	change(&copyNow)
	f.Copies[volume.LV] = copyNow
	return nil
}

// Copy is what the host has of a copy.
func (f *Storage) Copy(lv string) storage.Observed {
	f.Lock()
	defer f.Unlock()
	return f.Copies[lv]
}

// Observe returns Copies.
func (f *Storage) Observe(_ context.Context, volumes []storage.Volume) (map[string]storage.Observed, error) {
	f.Lock()
	defer f.Unlock()
	observed := map[string]storage.Observed{}
	for _, volume := range volumes {
		observed[volume.LV] = f.Copies[volume.LV]
	}
	return observed, nil
}

// Ensure makes the LV and brings the resource up secondary: a new copy is
// Inconsistent until a test says otherwise, a tiebreaker Diskless.
func (f *Storage) Ensure(ctx context.Context, volume storage.Volume) error {
	return f.step(ctx, "ensure", volume, func(copyNow *storage.Observed) {
		copyNow.LV = !volume.Diskless
		if volume.Resource != nil {
			copyNow.ResourceFileCurrent, copyNow.Quorum = true, true
			if copyNow.Role == storage.RoleDown {
				copyNow.Role = storage.RoleSecondary
			}
			switch {
			case volume.Diskless:
				copyNow.Disk = storage.DiskDiskless
			case copyNow.Disk == "":
				copyNow.Disk = "Inconsistent"
			}
		}
	})
}

// Promote makes the copy primary; with force, its data counts as current.
func (f *Storage) Promote(ctx context.Context, volume storage.Volume, force bool) error {
	name := "promote"
	if force {
		name = "promote --force"
	}
	return f.step(ctx, name, volume, func(copyNow *storage.Observed) {
		copyNow.Role = storage.RolePrimary
		if force {
			copyNow.Disk = storage.DiskUpToDate
		}
	})
}

// Mount mounts the copy.
func (f *Storage) Mount(ctx context.Context, volume storage.Volume, format bool) error {
	name := "mount"
	if format {
		name = "mount --format"
	}
	return f.step(ctx, name, volume, func(copyNow *storage.Observed) { copyNow.Mounted = true })
}

// Unmount unmounts the copy.
func (f *Storage) Unmount(ctx context.Context, volume storage.Volume) error {
	return f.step(ctx, "unmount", volume, func(copyNow *storage.Observed) { copyNow.Mounted = false })
}

// Demote makes the copy secondary.
func (f *Storage) Demote(ctx context.Context, volume storage.Volume) error {
	return f.step(ctx, "demote", volume, func(copyNow *storage.Observed) { copyNow.Role = storage.RoleSecondary })
}

// Remove takes the copy away.
func (f *Storage) Remove(ctx context.Context, volume storage.Volume) error {
	return f.step(ctx, "remove", volume, func(copyNow *storage.Observed) { *copyNow = storage.Observed{} })
}

// Trim records the call and fails while TrimFails is set.
func (f *Storage) Trim(_ context.Context, volume storage.Volume) error {
	f.Add("trim %s", volume.LV)
	f.Lock()
	defer f.Unlock()
	if f.TrimFails {
		return errors.New("fstrim timed out")
	}
	return nil
}

// ResolveSplitBrain records the call and connects the copy again.
func (f *Storage) ResolveSplitBrain(ctx context.Context, volume storage.Volume, discard bool) error {
	name := "resolve"
	if discard {
		name = "resolve --discard-my-data"
	}
	return f.step(ctx, name, volume, func(copyNow *storage.Observed) { copyNow.SplitBrain = false })
}

// ForgetPeer records the call.
func (f *Storage) ForgetPeer(_ context.Context, peer int) error {
	f.Add("forget-peer %d", peer)
	return nil
}

// PoolUsage is 100 GiB, DataPercent full.
func (f *Storage) PoolUsage(context.Context, string) (int64, float64, float64, error) {
	f.Lock()
	defer f.Unlock()
	if f.PoolFails {
		return 0, 0, 0, errors.New("lvs timed out")
	}
	return 100 << 30, f.DataPercent, 0, nil
}

// MountPoint is a directory under Dir.
func (f *Storage) MountPoint(volume storage.Volume) string {
	return filepath.Join(f.Dir, volume.LV)
}

// Volumes stands in for the volume reconciler: every copy is mounted under
// Dir, unless NotReady or Errors say otherwise.
type Volumes struct {
	sync.Mutex
	Dir      string
	NotReady map[string]bool
	Errors   map[string]error
}

// Mounted tells about a copy, by its LV.
func (f *Volumes) Mounted(lv string) (string, bool, error) {
	f.Lock()
	defer f.Unlock()
	if err := f.Errors[lv]; err != nil {
		return "", false, err
	}
	return filepath.Join(f.Dir, lv), !f.NotReady[lv], nil
}

// Docker stands in for Docker: it sends no events and has one container
// per stack.
type Docker struct {
	Calls
	// FailedUps and FailedDowns are how many compose ups and downs fail
	// before one works
	FailedUps, FailedDowns int
	// Down makes Ping fail as if refused, Hangs as if it timed out
	Down, Hangs bool
	// UpFiles and LeftOut are the files and the services left out of the
	// last compose up
	UpFiles, LeftOut []string
	// Listed and Details are what Docker lists and inspects
	Listed  []docker.Container
	Details map[string]docker.Details
	// DuringUp runs within each compose up, as a Docker restart would
	DuringUp func()
}

// Events sends nothing until ctx ends.
func (f *Docker) Events(ctx context.Context, _ string) (<-chan docker.Event, <-chan error) {
	events := make(chan docker.Event)
	go func() {
		<-ctx.Done()
		close(events)
	}()
	return events, make(chan error)
}

// Ping answers unless Down or Hangs is set.
func (f *Docker) Ping(ctx context.Context) error {
	f.Lock()
	down, hangs := f.Down, f.Hangs
	f.Unlock()
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case hangs:
		// the client's own timeout ran out
		return fmt.Errorf("ping Docker: %w", context.DeadlineExceeded)
	case down:
		return errors.New("connection refused")
	}
	return nil
}

// Start does nothing.
func (f *Docker) Start(context.Context, string) error { return nil }

// Restart does nothing.
func (f *Docker) Restart(context.Context, string) error { return nil }

// ComposeUp records the call; it fails while FailedUps is above zero.
func (f *Docker) ComposeUp(_ context.Context, stack string, files, leftOut []string) error {
	f.Add("up %s", stack)
	if f.DuringUp != nil {
		f.DuringUp()
	}
	f.Lock()
	defer f.Unlock()
	f.UpFiles, f.LeftOut = files, leftOut
	if f.FailedUps > 0 {
		f.FailedUps--
		return errors.New("pull access denied")
	}
	return nil
}

// ComposePull records the call.
func (f *Docker) ComposePull(_ context.Context, stack string, _, services []string) error {
	f.Add("pull %s %s", stack, strings.Join(services, " "))
	return nil
}

// PullMissing records the call.
func (f *Docker) PullMissing(_ context.Context, image string) error {
	f.Add("pull missing %s", image)
	return nil
}

// ComposeDown records the call; it fails while FailedDowns is above zero.
func (f *Docker) ComposeDown(_ context.Context, stack, _ string) error {
	f.Add("down %s", stack)
	f.Lock()
	defer f.Unlock()
	if f.FailedDowns > 0 {
		f.FailedDowns--
		return errors.New("the container does not stop")
	}
	return nil
}

// ComposePs lists one running container of the service app.
func (f *Docker) ComposePs(context.Context, string, string) (string, error) {
	return `{"Service":"app","State":"running","Health":"","Status":"Up 2 seconds"}` + "\n", nil
}

// ComposeLogs writes one line of app.
func (f *Docker) ComposeLogs(_ context.Context, _, _ string, _ bool, out io.Writer) error {
	_, err := fmt.Fprintln(out, "app-1  | hello")
	return err
}

// Login records the call.
func (f *Docker) Login(_ context.Context, registry, username, password string) error {
	f.Add("login %s %s %s", registry, username, password)
	return nil
}

// Containers are Listed.
func (f *Docker) Containers(context.Context, string) ([]docker.Container, error) {
	f.Lock()
	defer f.Unlock()
	return slices.Clone(f.Listed), nil
}

// Inspect returns a container's Details.
func (f *Docker) Inspect(_ context.Context, id string) (docker.Details, error) {
	f.Lock()
	defer f.Unlock()
	details, ok := f.Details[id]
	if !ok {
		return docker.Details{}, docker.ErrNoContainer
	}
	return details, nil
}

// Cluster is a state that applies what is proposed, as the leader would.
// Its host 0 is node0.
type Cluster struct {
	sync.Mutex
	t     *testing.T
	State *state.State
	// Keys seal the cluster's secrets
	Keys *datakey.Key
}

// NewCluster is a bootstrapped cluster with one host, node0, and the stack
// shop of the namespace default: one service that mounts the volume db
// (data generation 1), both placed on host 0.
func NewCluster(t *testing.T) *Cluster {
	t.Helper()
	keys, err := datakey.New()
	if err != nil {
		t.Fatal(err)
	}
	cluster := &Cluster{t: t, State: &state.State{}, Keys: keys}
	cluster.Apply(state.KindBootstrap, state.LeaderActor, state.Bootstrap{
		Keys:         state.ClusterKeys{CACertificate: []byte("ca"), CAKey: state.Sealed("k"), AgeIdentity: state.Sealed("a")},
		BootstrapKey: state.Sealed("j"),
		Host: state.Host{
			ID: 0, Name: "node0", Underlay: netip.MustParseAddr("10.0.0.2"), PublicAddress: netip.MustParseAddr("198.51.100.2"),
			ServerID: 40, WGPublicKey: "wg", CertificateSerial: "s0", CertificateNotAfter: time.Now().AddDate(1, 0, 0),
		},
	})
	cluster.Apply(state.KindApplyConfig, state.CLIActor, state.ApplyConfig{
		Spec: clusterspec.Config{
			Name: "prod", Endpoints: []string{"198.51.100.2"},
			Hetzner:        clusterspec.Hetzner{Token: "hetzner.token"},
			PrivateNetwork: "10.0.0.0/16",
			Ingress:        clusterspec.Ingress{FloatingIP: "203.0.113.10"},
			StorageClasses: map[string]clusterspec.StorageClass{"fast": {Pool: "vg-fast/thin"}},
		},
		Secrets: map[string]state.SealedSecret{"hetzner.token": {Value: state.Sealed("t"), Digest: "d"}},
	})
	cluster.Put(resource.KindVolume, "db", resource.VolumeSpec{Class: "fast", Size: 1 << 30, Replicas: 0})
	cluster.Put(resource.KindStack, "shop", UsingDB(stackspec.Service{}))
	cluster.PlaceVolume("db", 1)
	cluster.Place("shop")
	return cluster
}

// AddHost joins an active host with a token of its own.
func (c *Cluster) AddHost(id state.HostID) {
	c.t.Helper()
	token := "token-" + strconv.Itoa(int(id))
	c.Apply(state.KindAddJoinToken, state.CLIActor, state.AddJoinToken{
		ID: token, Hash: strings.Repeat("ab", 32), TTL: time.Hour,
	})
	c.Apply(state.KindAddHost, state.LeaderActor, state.AddHost{Token: token, Host: state.Host{
		ID: id, Name: "node" + strconv.Itoa(int(id)), Underlay: netip.AddrFrom4([4]byte{10, 0, 0, byte(id + 2)}),
		PublicAddress: netip.AddrFrom4([4]byte{198, 51, 100, byte(id + 2)}),
		WGPublicKey:   "wg", CertificateSerial: "s" + strconv.Itoa(int(id)), CertificateNotAfter: time.Now().AddDate(1, 0, 0),
		ServerID: 40 + int64(id), ServerChecked: true,
	}})
	c.Apply(state.KindSetHostPhase, state.LeaderActor, state.SetHostPhase{Host: id, Phase: state.Active})
}

// Read runs use under the lock.
func (c *Cluster) Read(use func(current *state.State)) {
	c.Lock()
	defer c.Unlock()
	use(c.State)
}

// SharedCopy copies afresh each time, and fails the test if a loop writes
// into its copy (statetest.ReadOnlyCopy).
func (c *Cluster) SharedCopy() (*state.State, error) {
	c.Lock()
	defer c.Unlock()
	return statetest.ReadOnlyCopy(c.t, c.State)
}

// Changed never fires: the tests run single passes.
func (c *Cluster) Changed() <-chan struct{} { return make(chan struct{}) }

// Propose applies a command, as committing it would.
func (c *Cluster) Propose(_ context.Context, cmd state.Command) (uint64, error) {
	c.Lock()
	defer c.Unlock()
	cmd.At = time.Now()
	index := c.State.Index + 1
	_, err := c.State.Apply(index, cmd)
	return index, err
}

// Apply commits a command and fails the test if the state refuses it.
func (c *Cluster) Apply(kind, actor string, payload any) {
	c.t.Helper()
	cmd, err := state.NewCommand(kind, time.Now(), actor, payload)
	if err != nil {
		c.t.Fatal(err)
	}
	if _, err := c.Propose(context.Background(), cmd); err != nil {
		c.t.Fatal(err)
	}
}

// Put applies a resource of the namespace default, as clowder apply does.
func (c *Cluster) Put(kind, name string, spec any) {
	c.t.Helper()
	encoded, err := json.Marshal(spec)
	if err != nil {
		c.t.Fatal(err)
	}
	c.Apply(state.KindPutResource, state.CLIActor, state.PutResource{
		Kind: kind, Namespace: names.Default, Name: name, Owner: state.OwnerCLI, Spec: encoded,
	})
}

// Remove deletes a resource of the namespace default.
func (c *Cluster) Remove(kind, name string) {
	c.t.Helper()
	c.Apply(state.KindDeleteResource, state.CLIActor, state.DeleteResource{
		Kind: kind, Namespace: names.Default, Name: name, Owner: state.OwnerCLI,
	})
}

// PutSecret applies a secret of the namespace default, sealed with the
// cluster's data key; its digest stands for one the API computes.
func (c *Cluster) PutSecret(name, value string) {
	c.t.Helper()
	sealed, err := c.Keys.Seal([]byte(value))
	if err != nil {
		c.t.Fatal(err)
	}
	c.Put(resource.KindSecret, name, state.SealedSecret{Value: sealed, Digest: "digest of " + value})
}

// Place puts a stack of the namespace default on host 0, as the placement
// does.
func (c *Cluster) Place(name string) {
	c.t.Helper()
	host := state.HostID(0)
	c.Apply(state.KindSetStackStatus, state.LeaderActor, state.SetStackStatus{
		ID: names.ID(names.Default, name), Status: state.StackStarting, Host: &host, PlannedAt: c.Index(),
	})
}

// Index is the state's index: the leader's commands of a test plan on the
// state as it is (state.SetStackStatus.PlannedAt).
func (c *Cluster) Index() uint64 {
	c.Lock()
	defer c.Unlock()
	return c.State.Index
}

// PlaceVolume places a volume of the namespace default on host 0 in a data
// generation, as the placement (the first) or a restore (the next) does.
func (c *Cluster) PlaceVolume(name string, generation int64) {
	c.t.Helper()
	var volume state.Volume
	c.Read(func(current *state.State) { volume = *current.Volumes[names.ID(names.Default, name)] })
	volume.DataGeneration, volume.Status = generation, state.VolumeOK
	volume.Copies = []state.VolumeCopy{{Host: 0, Class: "fast"}}
	c.Apply(state.KindPutVolume, state.LeaderActor, state.PutVolume{Volume: volume, PlannedAt: c.Index()})
}

// Stack returns a stack of the namespace default.
func (c *Cluster) Stack(name string) state.Stack {
	c.Lock()
	defer c.Unlock()
	return *c.State.Stacks[names.ID(names.Default, name)]
}
