package stacks

import (
	"context"
	"os"
	"slices"
	"testing"

	"github.com/webcodingcat/clowder/internal/agent/agenttest"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/resource"
	"github.com/webcodingcat/clowder/internal/stackspec"
	"github.com/webcodingcat/clowder/internal/state"
)

// handOver deletes shop and applies blog on shop's volume, so both land in
// the runner's next pass.
func handOver(cluster *agenttest.Cluster) {
	cluster.Remove(resource.KindStack, "shop")
	cluster.Put(resource.KindStack, "blog", agenttest.UsingDB(stackspec.Service{}))
	cluster.Place("blog")
}

// moveShopToLogs applies shop with the volume logs in place of db.
func moveShopToLogs(cluster *agenttest.Cluster) {
	cluster.Put(resource.KindVolume, "logs", resource.VolumeSpec{Class: "fast", Size: 1 << 30, Replicas: 0})
	cluster.PlaceVolume("logs", 1)
	cluster.Put(resource.KindStack, "shop", stackspec.Spec{Services: map[string]stackspec.Service{"app": {
		Image: "nginx", Volumes: []stackspec.Mount{{Volume: "logs", Path: "/logs"}},
	}}})
}

// logsLV is the LV of the volume default/logs in its first data generation.
var logsLV = names.VolumeName(names.ID(names.Default, "logs"), 1)

// mountedLVs reads the LVs shop's containers may hold.
func mountedLVs(t *testing.T, runner *Runner) []string {
	t.Helper()
	lvs, err := runner.files.readMountedLVs(shopProject)
	if err != nil {
		t.Fatal(err)
	}
	return lvs
}

func TestAVolumePassesOnlyOnceItsStackStopped(t *testing.T) {
	runner, cluster, docker, _ := newTestRunner(t)
	runner.passAndWait(context.Background())
	handOver(cluster)
	runner.passAndWait(context.Background())
	lines := docker.Lines()
	down := slices.Index(lines, "down "+shopProject)
	up := slices.Index(lines, "up default_blog")
	if down < 0 || up < 0 || down > up {
		t.Errorf("docker %q: shop must go down before blog comes up", lines)
	}
	if blog := cluster.Stack("blog"); blog.Status != state.StackRunning {
		t.Errorf("blog %s", blog.Status)
	}
	if _, err := os.Stat(runner.files.stackDir(shopProject)); !os.IsNotExist(err) {
		t.Errorf("the directory of the stopped stack is left: %v", err)
	}
}

func TestAStackWaitsWhileAStackThatFailedToStopUsesItsVolume(t *testing.T) {
	runner, cluster, docker, _ := newTestRunner(t)
	runner.passAndWait(context.Background())
	handOver(cluster)
	docker.FailedDowns = 1
	runner.passAndWait(context.Background())
	if docker.Count("up default_blog") != 0 {
		t.Fatalf("blog came up beside shop, which still runs: %q", docker.Lines())
	}
	if blog := cluster.Stack("blog"); blog.Status != state.StackStarting {
		t.Errorf("a stack that waits for a volume is %s: %q", blog.Status, blog.UpError)
	}
	runner.passAndWait(context.Background()) // shop stops now
	if docker.Count("up default_blog") != 1 || cluster.Stack("blog").Status != state.StackRunning {
		t.Errorf("docker %q, blog %s", docker.Lines(), cluster.Stack("blog").Status)
	}
}

func TestAStackWaitsWhenTheVolumesOfAStackThatFailedToStopDoNotRead(t *testing.T) {
	runner, cluster, docker, _ := newTestRunner(t)
	runner.passAndWait(context.Background())
	if lvs := mountedLVs(t, runner); !slices.Equal(lvs, []string{agenttest.DBLV(1)}) {
		t.Fatalf("the volumes shop mounts: %v", lvs)
	}
	// as a stack brought up before the file existed
	if err := os.Remove(runner.files.stackVolumesFile(shopProject)); err != nil {
		t.Fatal(err)
	}
	handOver(cluster)
	docker.FailedDowns = 1
	runner.passAndWait(context.Background())
	if docker.Count("up default_blog") != 0 {
		t.Fatalf("blog came up beside shop, whose volumes are not known: %q", docker.Lines())
	}
	runner.passAndWait(context.Background()) // shop stops now
	if docker.Count("up default_blog") != 1 {
		t.Errorf("docker %q", docker.Lines())
	}
}

func TestEveryStackWaitsWhileTheLeaversCannotBeListed(t *testing.T) {
	runner, cluster, docker, _ := newTestRunner(t)
	// a file where the stacks' directory belongs cannot be listed
	if err := os.WriteFile(runner.files.StacksDir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	runner.passAndWait(context.Background())
	if docker.Count("up "+shopProject) != 0 || cluster.Stack("shop").Status != state.StackStarting {
		t.Errorf("shop came up beside leavers no one knows: %q, %s", docker.Lines(), cluster.Stack("shop").Status)
	}
}

func TestALeaverStoppedBeforeACrashIsOnlyForgotten(t *testing.T) {
	runner, cluster, docker, _ := newTestRunner(t)
	runner.passAndWait(context.Background())
	cluster.Remove(resource.KindStack, "shop")
	// compose down worked and compose.yaml went, then the agent crashed
	// before the directory went
	if err := os.Remove(runner.files.stackFile(shopProject)); err != nil {
		t.Fatal(err)
	}
	runner.passAndWait(context.Background())
	if docker.Count("down "+shopProject) != 0 {
		t.Errorf("compose down ran again: %q", docker.Lines())
	}
	if _, err := os.Stat(runner.files.stackDir(shopProject)); !os.IsNotExist(err) {
		t.Errorf("the directory of the stopped stack is left: %v", err)
	}
}

func TestAFailedComposeUpKeepsTheOldVolumesInUse(t *testing.T) {
	runner, cluster, docker, _ := newTestRunner(t)
	runner.passAndWait(context.Background())
	moveShopToLogs(cluster)
	docker.FailedUps = 1
	runner.passAndWait(context.Background()) // shop's container still holds db
	if lvs := mountedLVs(t, runner); !slices.Equal(lvs, []string{agenttest.DBLV(1), logsLV}) {
		t.Errorf("shop may hold %v, want db's LV and logs'", lvs)
	}
	handOver(cluster)
	docker.FailedDowns = 1
	runner.passAndWait(context.Background())
	if docker.Count("up default_blog") != 0 {
		t.Fatalf("blog came up on db, which shop's old container still holds: %q", docker.Lines())
	}
	runner.passAndWait(context.Background()) // shop stops now
	if docker.Count("up default_blog") != 1 {
		t.Errorf("docker %q", docker.Lines())
	}
}

func TestAFailedComposeUpLeavesVolumesThatDoNotReadUnread(t *testing.T) {
	runner, cluster, docker, _ := newTestRunner(t)
	runner.passAndWait(context.Background())
	file := runner.files.stackVolumesFile(shopProject)
	if err := os.Remove(file); err != nil { // as a stack brought up before the file existed
		t.Fatal(err)
	}
	moveShopToLogs(cluster)
	docker.FailedUps = 1
	runner.passAndWait(context.Background())
	// the old container may hold any LV: listing only logs' would free db
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Errorf("the volumes of a stack with an unknown container were written: %v", err)
	}
	runner.passAndWait(context.Background())
	if lvs := mountedLVs(t, runner); !slices.Equal(lvs, []string{logsLV}) {
		t.Errorf("once up, shop may hold %v, want only logs' LV", lvs)
	}
}

func TestTheVolumesInUseShrinkOnceComposeUpWorked(t *testing.T) {
	runner, cluster, _, _ := newTestRunner(t)
	runner.passAndWait(context.Background())
	moveShopToLogs(cluster)
	runner.passAndWait(context.Background())
	if lvs := mountedLVs(t, runner); !slices.Equal(lvs, []string{logsLV}) {
		t.Errorf("shop may hold %v, want only logs' LV", lvs)
	}
}
