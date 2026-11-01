package storage

import (
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/webcodingcat/clowder/internal/command"
)

var update = flag.Bool("update", false, "write the golden files anew")

func TestResourceFilesMatchTheGoldenFiles(t *testing.T) {
	tests := map[string]Resource{
		"single.golden.res": {Name: "clw-shop-pgdata-b582c809", Minor: 3, Nodes: []Node{
			{Host: "node0", ID: 0, Disk: "/dev/vg-fast/clw-shop-pgdata-b582c809-g1"},
		}},
		"three.golden.res": {Name: "clw-shop-pgdata-b582c809", Minor: 3, ResyncRate: "30M", Nodes: []Node{
			{Host: "node0", ID: 0, Disk: "/dev/vg-fast/clw-shop-pgdata-b582c809-g1"},
			{Host: "node1", ID: 1, Disk: "/dev/vg-fast/clw-shop-pgdata-b582c809-g1"},
			{Host: "node2", ID: 2},
		}},
	}
	for name, resource := range tests {
		t.Run(name, func(t *testing.T) {
			rendered := RenderResource(resource)
			golden := filepath.Join("testdata", name)
			if *update {
				if err := os.WriteFile(golden, []byte(rendered), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("%v (run the test with -update to write it)", err)
			}
			if rendered != string(want) {
				t.Errorf("differs from %s; run with -update and review the diff:\n%s", golden, rendered)
			}
		})
	}
}

// fakeHost answers the driver's commands like a host would and records
// them.
type fakeHost struct {
	commands []string
	// existing are the things that are there: "lv", "metadata", "fs",
	// "mounted", "primary", "other device" (mounted at the copy's place).
	existing map[string]bool
}

// lvs lists the volume group's LVs: its thin pool, another volume's LV and
// the copy's if it is there.
func (f *fakeHost) lvs() string {
	names := "  thin\n  clw-shop-pgdata-g10\n"
	if f.existing["lv"] {
		names += "  clw-shop-pgdata-g1\n"
	}
	return names
}

func (f *fakeHost) run(_ context.Context, _ time.Duration, name string, args ...string) (string, error) {
	line := name + " " + strings.Join(args, " ")
	f.commands = append(f.commands, line)
	// the exit codes the tools use for "not there"
	switch {
	case name == "lvs":
		return f.lvs(), nil
	case name == "lvcreate":
		f.existing["lv"] = true
	case name == "lvremove":
		f.existing["lv"] = false
	case name == "umount":
		f.existing["mounted"] = false
	case name == "blkid" && !f.existing["fs"]:
		return "", &command.ExitError{Code: 2}
	case name == "blkid":
		return "ext4\n", nil
	case name == "mkfs.ext4":
		f.existing["fs"] = true
	case name == "findmnt" && !f.existing["mounted"]:
		return "", &command.ExitError{Code: 1}
	case name == "findmnt" && slices.Contains(args, "--source") && f.existing["other device"]:
		return "", &command.ExitError{Code: 1}
	case name == "mount":
		f.existing["mounted"] = true
	case line == "drbdadm role clw-shop-pgdata" && f.existing["primary"]:
		return "Primary\n", nil
	case line == "drbdadm role clw-shop-pgdata":
		return "Secondary\n", nil
	case strings.HasPrefix(line, "drbdadm primary"):
		f.existing["primary"] = true
	}
	return "", nil
}

func (f *fakeHost) ran(command string) bool {
	for _, line := range f.commands {
		if strings.HasPrefix(line, command) {
			return true
		}
	}
	return false
}

func newDriver(t *testing.T, host *fakeHost) Driver {
	t.Helper()
	dir := t.TempDir()
	return Driver{
		VolumesDir: filepath.Join(dir, "volumes"), StateDir: filepath.Join(dir, "storage"),
		ResourceDir: filepath.Join(dir, "drbd.d"), Run: host.run,
	}
}

func replicated() Volume {
	return Volume{
		ID: "shop/pgdata", LV: "clw-shop-pgdata-g1", Pool: "vg-fast/thin", SizeBytes: 1 << 30,
		Resource: &Resource{Name: "clw-shop-pgdata", Minor: 3, Nodes: []Node{
			{Host: "node0", ID: 0, Disk: "/dev/vg-fast/clw-shop-pgdata-g1"},
		}},
	}
}

func TestANewReplicatedVolumeGetsEverything(t *testing.T) {
	host := &fakeHost{existing: map[string]bool{}}
	driver := newDriver(t, host)
	if err := driver.Ensure(context.Background(), replicated()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"lvcreate --quiet --thin vg-fast/thin --virtualsize 1073741824b --name clw-shop-pgdata-g1",
		"drbdadm create-md --force --max-peers=4 clw-shop-pgdata",
		"drbdadm adjust clw-shop-pgdata",
		"drbdadm primary --force clw-shop-pgdata",
		"mkfs.ext4 -q /dev/drbd3",
		"mount -t ext4 /dev/drbd3 " + driver.MountPoint(replicated()),
		"mkdir -p " + filepath.Join(driver.MountPoint(replicated()), "data"),
	} {
		if !host.ran(want) {
			t.Errorf("did not run %q; ran:\n%s", want, strings.Join(host.commands, "\n"))
		}
	}
	if _, err := os.Stat(filepath.Join(driver.ResourceDir, "clw-shop-pgdata.res")); err != nil {
		t.Errorf("no resource file: %v", err)
	}
}

func TestEnsuringAgainChangesNothing(t *testing.T) {
	host := &fakeHost{existing: map[string]bool{}}
	driver := newDriver(t, host)
	if err := driver.Ensure(context.Background(), replicated()); err != nil {
		t.Fatal(err)
	}
	host.commands = nil
	if err := driver.Ensure(context.Background(), replicated()); err != nil {
		t.Fatal(err)
	}
	for _, never := range []string{"lvcreate", "drbdadm create-md", "drbdadm primary", "mkfs.ext4", "mount "} {
		if host.ran(never) {
			t.Errorf("ran %q again:\n%s", never, strings.Join(host.commands, "\n"))
		}
	}
}

func TestRemovingAgainSkipsWhatIsGone(t *testing.T) {
	host := &fakeHost{existing: map[string]bool{}}
	driver := newDriver(t, host)
	if err := driver.Ensure(context.Background(), replicated()); err != nil {
		t.Fatal(err)
	}
	if err := driver.Remove(context.Background(), replicated()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"umount", "drbdadm down clw-shop-pgdata", "lvremove --yes vg-fast/clw-shop-pgdata-g1"} {
		if !host.ran(want) {
			t.Errorf("did not run %q; ran:\n%s", want, strings.Join(host.commands, "\n"))
		}
	}
	// a crash before the copy's record was written again: removed once more
	host.commands = nil
	if err := driver.Remove(context.Background(), replicated()); err != nil {
		t.Fatalf("a second remove: %v", err)
	}
	for _, never := range []string{"umount", "drbdadm down", "lvremove"} {
		if host.ran(never) {
			t.Errorf("ran %q again:\n%s", never, strings.Join(host.commands, "\n"))
		}
	}
}

func TestAnLVWithoutKnownMetadataIsLeftAlone(t *testing.T) {
	// the LV is there, but this driver never wrote DRBD metadata on it
	host := &fakeHost{existing: map[string]bool{"lv": true}}
	driver := newDriver(t, host)
	err := driver.Ensure(context.Background(), replicated())
	if err == nil || !strings.Contains(err.Error(), "never wrote its DRBD metadata") {
		t.Errorf("want a refusal, got %v", err)
	}
	if host.ran("drbdadm create-md") {
		t.Error("the metadata of an unknown LV was overwritten")
	}
}

func TestEnsurePicksUpAfterACrashBeforeTheMetadata(t *testing.T) {
	host := &fakeHost{existing: map[string]bool{}}
	driver := newDriver(t, host)
	// the first run dies right after lvcreate
	crashing := driver
	crashing.Run = func(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error) {
		if name == "drbdadm" {
			return "", errors.New("power lost")
		}
		return host.run(ctx, timeout, name, args...)
	}
	if err := crashing.Ensure(context.Background(), replicated()); err == nil {
		t.Fatal("the crash went unnoticed")
	}
	if err := driver.Ensure(context.Background(), replicated()); err != nil {
		t.Fatalf("the LV clowder made itself must get its metadata after a crash: %v", err)
	}
	if !host.ran("drbdadm create-md") {
		t.Error("no metadata written")
	}
}

func TestOnlyTheToolsOwnExitCodeMeansNotThere(t *testing.T) {
	tests := []struct {
		name  string
		tool  string
		never string
	}{
		{"blkid times out", "blkid", "mkfs.ext4"},
		{"lvs times out", "lvs", "lvcreate"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			host := &fakeHost{existing: map[string]bool{"lv": true, "fs": false}}
			driver := newDriver(t, host)
			volume := replicated()
			volume.Resource = nil
			driver.Run = func(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error) {
				if name == test.tool {
					host.commands = append(host.commands, name)
					return "", errors.New("no answer within 1m0s")
				}
				return host.run(ctx, timeout, name, args...)
			}
			if err := driver.Ensure(context.Background(), volume); err == nil {
				t.Error("Ensure went on after a timeout")
			}
			if host.ran(test.never) {
				t.Errorf("ran %s after %s timed out", test.never, test.tool)
			}
		})
	}
}

func TestAPlainVolumeHasNoDRBD(t *testing.T) {
	host := &fakeHost{existing: map[string]bool{}}
	driver := newDriver(t, host)
	volume := replicated()
	volume.Resource = nil
	if err := driver.Ensure(context.Background(), volume); err != nil {
		t.Fatal(err)
	}
	if host.ran("drbdadm") || !host.ran("mkfs.ext4 -q /dev/vg-fast/clw-shop-pgdata-g1") {
		t.Errorf("ran:\n%s", strings.Join(host.commands, "\n"))
	}
}

func TestEachGenerationHasAMountPointOfItsOwn(t *testing.T) {
	driver := newDriver(t, &fakeHost{existing: map[string]bool{}})
	first, second := replicated(), replicated()
	second.LV = "clw-shop-pgdata-g2"
	if driver.MountPoint(first) == driver.MountPoint(second) {
		t.Errorf("two generations share %s", driver.MountPoint(first))
	}
}

func TestAForeignPlainLVIsNotFormatted(t *testing.T) {
	// the LV is there without a file system, but this driver never made it
	host := &fakeHost{existing: map[string]bool{"lv": true}}
	driver := newDriver(t, host)
	volume := replicated()
	volume.Resource = nil
	err := driver.Ensure(context.Background(), volume)
	if err == nil || !strings.Contains(err.Error(), "clowder did not create LV clw-shop-pgdata-g1") {
		t.Errorf("want a refusal, got %v", err)
	}
	if host.ran("mkfs.ext4") {
		t.Error("an LV clowder did not make was formatted")
	}
}

func TestAForeignFileSystemIsNotTouched(t *testing.T) {
	host := &fakeHost{existing: map[string]bool{"lv": true}}
	driver := newDriver(t, host)
	host.existing["fs"] = true
	volume := replicated()
	volume.Resource = nil
	driver.Run = func(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error) {
		if name == "blkid" {
			return "xfs\n", nil
		}
		return host.run(ctx, timeout, name, args...)
	}
	if err := driver.Ensure(context.Background(), volume); err == nil || !strings.Contains(err.Error(), "holds xfs") {
		t.Errorf("want a refusal, got %v", err)
	}
}

func TestAnotherDeviceAtTheMountPointStopsEnsure(t *testing.T) {
	host := &fakeHost{existing: map[string]bool{}}
	driver := newDriver(t, host)
	volume := replicated()
	volume.Resource = nil
	if err := driver.Ensure(context.Background(), volume); err != nil {
		t.Fatal(err)
	}
	host.existing["other device"] = true
	host.commands = nil
	if err := driver.Ensure(context.Background(), volume); err == nil || !strings.Contains(err.Error(), "another device") {
		t.Errorf("want a refusal, got %v", err)
	}
	if host.ran("mkdir") {
		t.Error("data/ was made on another device")
	}
}

func TestAnLVOfASimilarNameIsNotTheCopys(t *testing.T) {
	// the volume group holds clw-shop-pgdata-g10, not -g1
	host := &fakeHost{existing: map[string]bool{}}
	driver := newDriver(t, host)
	volume := replicated()
	volume.Resource = nil
	if err := driver.Ensure(context.Background(), volume); err != nil {
		t.Fatal(err)
	}
	if !host.ran("lvcreate") {
		t.Error("the copy's LV was taken as there")
	}
}

func TestPoolUsageReadsLVS(t *testing.T) {
	driver := Driver{Run: func(context.Context, time.Duration, string, ...string) (string, error) {
		return "  107374182400,12.50,3.01\n", nil
	}}
	size, data, metadata, err := driver.PoolUsage(context.Background(), "vg-fast/thin")
	if err != nil || size != 100<<30 || data != 12.5 || metadata != 3.01 {
		t.Errorf("%d %v %v %v", size, data, metadata, err)
	}
}
