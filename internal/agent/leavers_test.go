package agent

import (
	"context"
	"os"
	"slices"
	"testing"

	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/resource"
	"github.com/webcodingcat/clowder/internal/stackspec"
	"github.com/webcodingcat/clowder/internal/state"
)

// handOver deletes shop and applies blog on shop's volume, so both land in
// the runner's next pass.
func handOver(cluster *testCluster) {
	cluster.t.Helper()
	cluster.remove(resource.KindStack, "shop")
	cluster.put(resource.KindStack, "blog", usingDB(stackspec.Service{}))
	cluster.place("blog")
}

// moveShopToLogs applies shop with the volume logs in place of db.
func moveShopToLogs(cluster *testCluster) {
	cluster.t.Helper()
	cluster.put(resource.KindVolume, "logs", resource.VolumeSpec{Class: "fast", Size: 1 << 30, Replicas: 0})
	placeVolume(cluster, "logs", 1)
	cluster.put(resource.KindStack, "shop", stackspec.Spec{Services: map[string]stackspec.Service{"app": {
		Image: "nginx", Volumes: []stackspec.Mount{{Volume: "logs", Path: "/logs"}},
	}}})
}

// logsLV is the LV of the volume default/logs in its first data generation.
var logsLV = names.VolumeName(names.ID(names.Default, "logs"), 1)

// mountedLVs reads the LVs shop's containers may hold.
func mountedLVs(t *testing.T, runner *stackRunner) []string {
	t.Helper()
	lvs, err := runner.readMountedLVs(shopProject)
	if err != nil {
		t.Fatal(err)
	}
	return lvs
}

func TestAVolumePassesOnlyOnceItsStackStopped(t *testing.T) {
	runner, cluster, docker, storage := newTestRunner(t)
	runner.pass(context.Background())
	handOver(cluster)
	runner.pass(context.Background())
	docker.mu.Lock()
	down := slices.Index(docker.lines, "down "+shopProject)
	up := slices.Index(docker.lines, "up default_blog")
	docker.mu.Unlock()
	if down < 0 || up < 0 || down > up {
		t.Errorf("docker %q: shop must go down before blog comes up", docker.lines)
	}
	if storage.count("remove "+dbLV(1)) != 0 || cluster.stack("blog").Status != state.StackRunning {
		t.Errorf("storage %q, blog %s", storage.lines, cluster.stack("blog").Status)
	}
	if _, err := os.Stat(runner.files.stackDir(shopProject)); !os.IsNotExist(err) {
		t.Errorf("the directory of the stopped stack is left: %v", err)
	}
}

func TestAStackWaitsWhileAStackThatFailedToStopUsesItsVolume(t *testing.T) {
	runner, cluster, docker, _ := newTestRunner(t)
	runner.pass(context.Background())
	handOver(cluster)
	docker.failedDowns = 1
	runner.pass(context.Background())
	if docker.count("up default_blog") != 0 {
		t.Fatalf("blog came up beside shop, which still runs: %q", docker.lines)
	}
	if blog := cluster.stack("blog"); blog.Status != state.StackStarting {
		t.Errorf("a stack that waits for a volume is %s: %q", blog.Status, blog.UpError)
	}
	runner.pass(context.Background()) // shop stops now
	if docker.count("up default_blog") != 1 || cluster.stack("blog").Status != state.StackRunning {
		t.Errorf("docker %q, blog %s", docker.lines, cluster.stack("blog").Status)
	}
}

func TestAStackWaitsWhenTheVolumesOfAStackThatFailedToStopDoNotRead(t *testing.T) {
	runner, cluster, docker, _ := newTestRunner(t)
	runner.pass(context.Background())
	if lvs := mountedLVs(t, runner); !slices.Equal(lvs, []string{dbLV(1)}) {
		t.Fatalf("the volumes shop mounts: %v", lvs)
	}
	// as a stack brought up before the file existed
	if err := os.Remove(runner.files.stackVolumesFile(shopProject)); err != nil {
		t.Fatal(err)
	}
	handOver(cluster)
	docker.failedDowns = 1
	runner.pass(context.Background())
	if docker.count("up default_blog") != 0 {
		t.Fatalf("blog came up beside shop, whose volumes are not known: %q", docker.lines)
	}
	runner.pass(context.Background()) // shop stops now
	if docker.count("up default_blog") != 1 {
		t.Errorf("docker %q", docker.lines)
	}
}

func TestEveryStackWaitsWhileTheLeaversCannotBeListed(t *testing.T) {
	runner, cluster, docker, _ := newTestRunner(t)
	// a file where the stacks' directory belongs cannot be listed
	if err := os.WriteFile(runner.files.stacksDir(), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	runner.pass(context.Background())
	if docker.count("up "+shopProject) != 0 || cluster.stack("shop").Status != state.StackStarting {
		t.Errorf("shop came up beside leavers no one knows: %q, %s", docker.lines, cluster.stack("shop").Status)
	}
}

func TestALeaverStoppedBeforeACrashIsOnlyForgotten(t *testing.T) {
	runner, cluster, docker, _ := newTestRunner(t)
	runner.pass(context.Background())
	cluster.remove(resource.KindStack, "shop")
	// compose down worked and compose.yaml went, then the agent crashed
	// before the directory went
	if err := os.Remove(runner.files.stackFile(shopProject)); err != nil {
		t.Fatal(err)
	}
	runner.pass(context.Background())
	if docker.count("down "+shopProject) != 0 {
		t.Errorf("compose down ran again: %q", docker.lines)
	}
	if _, err := os.Stat(runner.files.stackDir(shopProject)); !os.IsNotExist(err) {
		t.Errorf("the directory of the stopped stack is left: %v", err)
	}
}

func TestAFailedComposeUpKeepsTheOldVolumesInUse(t *testing.T) {
	runner, cluster, docker, _ := newTestRunner(t)
	runner.pass(context.Background())
	moveShopToLogs(cluster)
	docker.failedUps = 1
	runner.pass(context.Background()) // shop's container still holds db
	if lvs := mountedLVs(t, runner); !slices.Equal(lvs, []string{dbLV(1), logsLV}) {
		t.Errorf("shop may hold %v, want db's LV and logs'", lvs)
	}
	handOver(cluster)
	docker.failedDowns = 1
	runner.pass(context.Background())
	if docker.count("up default_blog") != 0 {
		t.Fatalf("blog came up on db, which shop's old container still holds: %q", docker.lines)
	}
	runner.pass(context.Background()) // shop stops now
	if docker.count("up default_blog") != 1 {
		t.Errorf("docker %q", docker.lines)
	}
}

func TestAFailedComposeUpLeavesVolumesThatDoNotReadUnread(t *testing.T) {
	runner, cluster, docker, _ := newTestRunner(t)
	runner.pass(context.Background())
	file := runner.files.stackVolumesFile(shopProject)
	if err := os.Remove(file); err != nil { // as a stack brought up before the file existed
		t.Fatal(err)
	}
	moveShopToLogs(cluster)
	docker.failedUps = 1
	runner.pass(context.Background())
	// the old container may hold any LV: listing only logs' would free db
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Errorf("the volumes of a stack with an unknown container were written: %v", err)
	}
	runner.pass(context.Background())
	if lvs := mountedLVs(t, runner); !slices.Equal(lvs, []string{logsLV}) {
		t.Errorf("once up, shop may hold %v, want only logs' LV", lvs)
	}
}

func TestTheVolumesInUseShrinkOnceComposeUpWorked(t *testing.T) {
	runner, cluster, _, _ := newTestRunner(t)
	runner.pass(context.Background())
	moveShopToLogs(cluster)
	runner.pass(context.Background())
	if lvs := mountedLVs(t, runner); !slices.Equal(lvs, []string{logsLV}) {
		t.Errorf("shop may hold %v, want only logs' LV", lvs)
	}
}
