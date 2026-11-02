package volumes

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/webcodingcat/clowder/internal/agent/agenttest"
	"github.com/webcodingcat/clowder/internal/agent/loop"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/resource"
	"github.com/webcodingcat/clowder/internal/stackspec"
	"github.com/webcodingcat/clowder/internal/state"
	"github.com/webcodingcat/clowder/internal/storage"
)

// heldLVs stands in for the stacks' lists of the LVs they hold.
type heldLVs struct {
	sync.Mutex
	lvs map[string]bool
	err error
}

func (h *heldLVs) read() (map[string]bool, error) {
	h.Lock()
	defer h.Unlock()
	return h.lvs, h.err
}

func (h *heldLVs) set(lvs map[string]bool, err error) {
	h.Lock()
	defer h.Unlock()
	h.lvs, h.err = lvs, err
}

// testReconciler is a reconciler on host 0 of agenttest.NewCluster: the
// stack shop uses the plain volume db (data generation 1), both placed on
// host 0.
type testReconciler struct {
	*Reconciler
	cluster *agenttest.Cluster
	storage *agenttest.Storage
	held    *heldLVs
	// readies counts how often the stack runner heard of a mounted copy
	readies *atomic.Int32
}

func newTestReconciler(t *testing.T) testReconciler {
	t.Helper()
	cluster := agenttest.NewCluster(t)
	hostStorage, held, readies := &agenttest.Storage{Dir: t.TempDir()}, &heldLVs{}, &atomic.Int32{}
	dir := t.TempDir()
	reconciler := New(Parts{
		Files:   Files{RecordsDir: filepath.Join(dir, "volume-records"), LastTrim: filepath.Join(dir, "last-trim")},
		Host:    0,
		Cluster: cluster, Storage: hostStorage, Held: held.read, Signal: loop.NewSignal(),
		StacksReady: func() { readies.Add(1) }, Propose: cluster.Propose,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return testReconciler{reconciler, cluster, hostStorage, held, readies}
}

// settle runs passes and waits for the jobs they start, until the copies
// reached what the state says (three rounds are more than any copy needs).
func (r testReconciler) settle() {
	for range 3 {
		r.pass(context.Background())
		r.jobs.Wait()
	}
}

// placeData places the replicated volume data on host 0, with DRBD minor 5.
func placeData(cluster *agenttest.Cluster) {
	cluster.Put(resource.KindVolume, "data", resource.VolumeSpec{Class: "fast", Size: 1 << 30, Replicas: 2})
	var volume state.Volume
	cluster.Read(func(current *state.State) { volume = *current.Volumes[names.ID(names.Default, "data")] })
	minor := 5
	volume.Minor, volume.Status = &minor, state.VolumeOK
	volume.Copies = []state.VolumeCopy{{Host: 0, Class: "fast"}}
	cluster.Apply(state.KindPutVolume, state.LeaderActor, state.PutVolume{Volume: volume, PlannedAt: cluster.Index()})
}

// placeBlog places the stack blog, which mounts data, on host 0.
func placeBlog(cluster *agenttest.Cluster) {
	cluster.Put(resource.KindStack, "blog", stackspec.Spec{Services: map[string]stackspec.Service{"app": {
		Image: "nginx", Volumes: []stackspec.Mount{{Volume: "data", Path: "/data"}},
	}}})
	cluster.Place("blog")
}

var dataLV = names.VolumeName(names.ID(names.Default, "data"), 1)

func initialized(cluster *agenttest.Cluster, name string) bool {
	var done bool
	cluster.Read(func(current *state.State) { done = current.Volumes[names.ID(names.Default, name)].Initialized })
	return done
}

func TestAVolumeAStackUsesIsMadeFormattedAndMounted(t *testing.T) {
	r := newTestReconciler(t)
	lv := agenttest.DBLV(1)
	if _, ready, err := r.Mounted(lv); ready || err != nil {
		t.Fatalf("ready %v, %v before any pass", ready, err)
	}
	r.settle()
	for _, want := range []string{"ensure " + lv, "mount --format " + lv} {
		if r.storage.Count(want) != 1 {
			t.Errorf("want %q once; storage %q", want, r.storage.Lines())
		}
	}
	if !initialized(r.cluster, "db") {
		t.Error("the state does not know the volume holds data")
	}
	dir, ready, err := r.Mounted(lv)
	if !ready || err != nil || dir != filepath.Join(r.storage.Dir, lv) {
		t.Errorf("Mounted: %q, %v, %v", dir, ready, err)
	}
	if r.readies.Load() != 1 {
		t.Errorf("the stack runner heard %d times of the mounted copy, want 1", r.readies.Load())
	}

	// with everything in place, a pass runs no step
	calls := len(r.storage.Lines())
	r.settle()
	if len(r.storage.Lines()) != calls {
		t.Errorf("a pass with nothing to do ran %q", r.storage.Lines()[calls:])
	}
}

func TestACopyWithoutAStackIsSecondaryAndUnmounted(t *testing.T) {
	r := newTestReconciler(t)
	placeData(r.cluster)
	r.settle()
	if got := r.storage.Copy(dataLV); got.Role != storage.RoleSecondary || got.Mounted {
		t.Fatalf("a copy no stack here uses: %+v", got)
	}
	if strings.Contains(strings.Join(r.storage.Lines(), "\n"), "promote") || initialized(r.cluster, "data") {
		t.Errorf("a copy no stack uses was promoted: %q", r.storage.Lines())
	}

	// the first stack makes it the volume's first copy
	placeBlog(r.cluster)
	r.settle()
	if r.storage.Count("promote --force "+dataLV) != 1 || r.storage.Count("mount --format "+dataLV) != 1 {
		t.Errorf("storage %q", r.storage.Lines())
	}
	if !initialized(r.cluster, "data") {
		t.Error("not initialized")
	}

	// the stack goes: the copy is given up for a stack elsewhere
	r.cluster.Remove(resource.KindStack, "blog")
	r.settle()
	if got := r.storage.Copy(dataLV); got.Role != storage.RoleSecondary || got.Mounted {
		t.Errorf("the copy after its stack left: %+v", got)
	}
	if _, ready, _ := r.Mounted(dataLV); ready {
		t.Error("a secondary copy counts as mounted")
	}

	// the next stack finds the data: no --force, no new file system
	placeBlog(r.cluster)
	r.settle()
	if r.storage.Count("promote "+dataLV) != 1 || r.storage.Count("mount "+dataLV) != 1 ||
		r.storage.Count("promote --force "+dataLV) != 1 || r.storage.Count("mount --format "+dataLV) != 1 {
		t.Errorf("storage %q", r.storage.Lines())
	}
}

func TestACopyAStackMayStillHoldIsLeftAlone(t *testing.T) {
	r := newTestReconciler(t)
	lv := agenttest.DBLV(1)
	r.settle()
	r.held.set(map[string]bool{lv: true}, nil)
	r.cluster.Remove(resource.KindStack, "shop")
	r.settle()
	if r.storage.Count("unmount "+lv) != 0 {
		t.Errorf("a copy a stack still holds was unmounted: %q", r.storage.Lines())
	}

	// any copy, while the stacks' lists do not read
	r.held.set(nil, errors.New("read volumes.json: unexpected end of JSON input"))
	r.settle()
	if r.storage.Count("unmount "+lv) != 0 {
		t.Errorf("a copy was unmounted while the stacks' lists did not read: %q", r.storage.Lines())
	}

	r.held.set(nil, nil)
	r.settle()
	if r.storage.Count("unmount "+lv) != 1 {
		t.Errorf("storage %q", r.storage.Lines())
	}
}

func TestAJobThatHangsHoldsUpNoOtherVolume(t *testing.T) {
	r := newTestReconciler(t)
	lv := agenttest.DBLV(1)
	hold := make(chan struct{})
	r.storage.Lock()
	r.storage.Hold = map[string]chan struct{}{lv: hold}
	r.storage.Unlock()
	placeData(r.cluster)

	done := make(chan struct{})
	go func() {
		r.pass(context.Background())
		r.pass(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a pass waited for a job")
	}
	r.storage.WaitFor(t, "ensure "+dataLV)
	if r.storage.Count("ensure "+lv) != 1 {
		t.Errorf("a second job started on a busy volume: %q", r.storage.Lines())
	}
	if _, ready, _ := r.Mounted(lv); ready {
		t.Error("ready while its job hangs")
	}

	close(hold)
	r.settle()
	if _, ready, _ := r.Mounted(lv); !ready {
		t.Errorf("not ready once the job went on: %q", r.storage.Lines())
	}
}

func TestAStepThatFailsReachesTheStack(t *testing.T) {
	r := newTestReconciler(t)
	lv := agenttest.DBLV(1)
	r.storage.Lock()
	r.storage.Fails = map[string]error{lv: errors.New("mkfs.ext4: no answer within 10m0s")}
	r.storage.Unlock()
	r.settle()
	if _, ready, err := r.Mounted(lv); ready || err == nil || !strings.Contains(err.Error(), "ensure: mkfs.ext4") {
		t.Errorf("ready %v, %v", ready, err)
	}
}

func TestAMissingPlainCopyOfDataIsNeverMadeAnew(t *testing.T) {
	r := newTestReconciler(t)
	lv := agenttest.DBLV(1)
	r.settle()
	r.storage.Lock()
	delete(r.storage.Copies, lv) // someone removed the LV
	r.storage.Unlock()
	r.settle()
	if r.storage.Count("ensure "+lv) != 1 {
		t.Errorf("an empty LV took the place of the data: %q", r.storage.Lines())
	}
	if _, _, err := r.Mounted(lv); !errors.Is(err, errDataGone) {
		t.Errorf("want errDataGone, got %v", err)
	}
}

func TestATiebreakerHereIsNotKeptYet(t *testing.T) {
	r := newTestReconciler(t)
	r.cluster.Put(resource.KindVolume, "data", resource.VolumeSpec{Class: "fast", Size: 1 << 30, Replicas: 2})
	var volume state.Volume
	r.cluster.Read(func(current *state.State) { volume = *current.Volumes[names.ID(names.Default, "data")] })
	minor := 5
	volume.Minor, volume.Status = &minor, state.VolumeOK
	volume.Copies = []state.VolumeCopy{{Host: 0, Tiebreaker: true}}
	r.cluster.Apply(state.KindPutVolume, state.LeaderActor, state.PutVolume{Volume: volume, PlannedAt: r.cluster.Index()})
	r.settle()
	if _, _, err := r.Mounted(dataLV); !errors.Is(err, errTiebreakerHere) {
		t.Errorf("want errTiebreakerHere, got %v", err)
	}
	if r.storage.Count("ensure "+dataLV) != 0 {
		t.Errorf("storage %q", r.storage.Lines())
	}
}

func TestANewDataGenerationReplacesTheCopy(t *testing.T) {
	r := newTestReconciler(t)
	r.settle()
	r.cluster.PlaceVolume("db", 2) // a restore
	r.settle()
	old, current := agenttest.DBLV(1), agenttest.DBLV(2)
	if r.storage.Count("mount --format "+current) != 1 || r.storage.Count("remove "+old) != 1 ||
		r.storage.Count("remove "+current) != 0 {
		t.Errorf("storage %q", r.storage.Lines())
	}
	if retired := r.cluster.State.Retired; len(retired) != 0 {
		t.Errorf("the removed copy is not forgotten: %v", retired)
	}
	recorded, err := r.recordedCopies()
	if err != nil || len(recorded["default/db"]) != 1 || recorded["default/db"][0].LV != current {
		t.Errorf("recorded %+v, %v", recorded, err)
	}
}

func TestACopyTheStateLostIsKept(t *testing.T) {
	r := newTestReconciler(t)
	r.settle()
	// a bug, not a command, takes the volume and its stack out of the state
	r.cluster.Lock()
	delete(r.cluster.State.Volumes, "default/db")
	delete(r.cluster.State.Stacks, "default/shop")
	r.cluster.Unlock()
	r.settle()
	if r.storage.Count("remove "+agenttest.DBLV(1)) != 0 {
		t.Errorf("a copy that is neither placed nor retired was removed: %q", r.storage.Lines())
	}
	if !r.unexplained[agenttest.DBLV(1)] {
		t.Error("no warning about the kept copy")
	}
}

func TestAVolumeOutlivesItsStackAndGoesWithItsDelete(t *testing.T) {
	r := newTestReconciler(t)
	lv := agenttest.DBLV(1)
	r.settle()
	r.cluster.Remove(resource.KindStack, "shop")
	r.settle()
	if r.storage.Count("remove "+lv) != 0 || !r.storage.Copy(lv).LV {
		t.Errorf("the copy left with its stack: %q", r.storage.Lines())
	}

	// a copy a stack still holds waits for it
	r.held.set(map[string]bool{lv: true}, nil)
	r.cluster.Remove(resource.KindVolume, "db")
	r.settle()
	if r.storage.Count("remove "+lv) != 0 {
		t.Errorf("a held copy was removed: %q", r.storage.Lines())
	}
	r.held.set(nil, nil)
	r.settle()
	if r.storage.Count("remove "+lv) != 1 {
		t.Errorf("storage %q", r.storage.Lines())
	}
	if retired := r.cluster.State.Retired; len(retired) != 0 {
		t.Errorf("the removed copy is not forgotten: %v", retired)
	}
	if _, err := os.Stat(r.recordFile("default/db")); !os.IsNotExist(err) {
		t.Errorf("the record of the removed copy is left: %v", err)
	}
}

func TestTheRecordKeepsEachCopyOnce(t *testing.T) {
	r := newTestReconciler(t)
	first := storage.Volume{ID: "default/db", LV: agenttest.DBLV(1), Pool: "vg-fast/thin", SizeBytes: 1 << 30}
	second := storage.Volume{ID: "default/db", LV: agenttest.DBLV(2), Pool: "vg-fast/thin", SizeBytes: 1 << 30}
	logs := storage.Volume{ID: "default/logs", LV: "logs-1", Pool: "vg-fast/thin", SizeBytes: 1 << 20}
	grown := first
	grown.SizeBytes = 2 << 30
	// recorded again, as each ensure does, and once grown: still one entry
	for _, volume := range []storage.Volume{second, logs, first, first, grown} {
		if err := r.recordCopy(volume); err != nil {
			t.Fatal(err)
		}
	}

	recorded, err := r.recordedCopies()
	if err != nil {
		t.Fatal(err)
	}
	db := recorded["default/db"]
	if len(recorded) != 2 || len(db) != 2 || len(recorded["default/logs"]) != 1 {
		t.Fatalf("recorded %+v", recorded)
	}
	if db[0].LV != first.LV || db[0].SizeBytes != grown.SizeBytes || db[1].LV != second.LV {
		t.Errorf("the copies of db, sorted by LV, the first grown: %+v", db)
	}

	for _, volume := range []storage.Volume{first, second} {
		if err := r.forgetCopy(volume); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(r.recordFile("default/db")); !os.IsNotExist(err) {
		t.Errorf("the record without copies stayed: %v", err)
	}
}

func TestAHostWithoutRecordsHasNoCopies(t *testing.T) {
	r := newTestReconciler(t)
	recorded, err := r.recordedCopies()
	if err != nil || len(recorded) != 0 {
		t.Errorf("recorded %v, %v", recorded, err)
	}
}

func TestARecordThatDoesNotReadIsNeitherUsedNorOverwritten(t *testing.T) {
	r := newTestReconciler(t)
	r.settle()
	file := r.recordFile("default/db")
	if err := os.WriteFile(file, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.recordedCopies(); err == nil {
		t.Error("the copies were listed without the broken record")
	}
	// overwriting it would forget the copies it lists, and with them data
	r.cluster.PlaceVolume("db", 2)
	r.settle()
	if content, err := os.ReadFile(file); err != nil || string(content) != "{not json" {
		t.Errorf("the broken record was overwritten: %q, %v", content, err)
	}
	if r.storage.Count("ensure "+agenttest.DBLV(2)) != 0 || r.storage.Count("remove "+agenttest.DBLV(1)) != 0 {
		t.Errorf("storage calls %q", r.storage.Lines())
	}
}

func TestTrimRunsOnceAWeekAcrossRestartsOnTheMountedCopies(t *testing.T) {
	r := newTestReconciler(t)
	done, cancel := context.WithCancel(context.Background())
	cancel() // one round of the loop, then it ends
	trims := func() int {
		r.Trim(done)
		return r.storage.Count("trim " + agenttest.DBLV(1))
	}
	if got := trims(); got != 0 {
		t.Fatalf("%d trims before the first pass, want 0", got)
	}

	placeData(r.cluster) // secondary: not mounted, not trimmed
	r.settle()
	if got := trims(); got != 1 {
		t.Fatalf("%d trims at the first start, want 1; storage %q", got, r.storage.Lines())
	}
	if got := trims(); got != 1 {
		t.Errorf("%d trims after a restart within the week, want 1", got)
	}
	weekAgo := time.Now().Add(-trimInterval)
	if err := os.Chtimes(r.files.LastTrim, weekAgo, weekAgo); err != nil {
		t.Fatal(err)
	}
	r.storage.Lock()
	r.storage.TrimFails = true
	r.storage.Unlock()
	if got := trims(); got != 2 {
		t.Errorf("%d trims a week later, want 2", got)
	}
	// a failed trim counts too: the next try is in a week, not in an hour
	if got := trims(); got != 2 {
		t.Errorf("%d trims after a failed trim, want 2", got)
	}
}
