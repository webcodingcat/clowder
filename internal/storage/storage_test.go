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
// them, for the one copy of replicated() or plain().
type fakeHost struct {
	commands []string
	// existing are the things that are there: "lv", "up" (the resource),
	// "primary", "fs", "mounted", "other device" (mounted at the copy's
	// place).
	existing map[string]bool
	// mountPoint is the copy's, which findmnt lists while it is mounted
	mountPoint string
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

// status is drbdsetup's JSON: the resource while it is up.
func (f *fakeHost) status() string {
	if !f.existing["up"] {
		return "[\n]\n"
	}
	role := "Secondary"
	if f.existing["primary"] {
		role = "Primary"
	}
	devices := ""
	if f.existing["diskless"] {
		devices = `, "devices": [{"disk-state": "Diskless"}]`
	}
	if f.existing["standalone"] {
		devices += `, "connections": [{"peer-node-id": 1, "connection-state": "StandAlone"}]`
	}
	return `[{"name": "clw-shop-pgdata", "node-id": 0, "role": "` + role + `", "suspended": false` + devices + `}]`
}

// mountTable is findmnt's list: the root and, while mounted, the copy's.
func (f *fakeHost) mountTable() string {
	table := "/ /dev/sda1\n"
	switch {
	case f.existing["mounted"] && f.existing["other device"]:
		table += f.mountPoint + " /dev/sdz\n"
	case f.existing["mounted"]:
		table += f.mountPoint + " /dev/drbd3\n"
	}
	return table
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
	case line == "drbdsetup status --json":
		return f.status(), nil
	case line == "drbdadm adjust clw-shop-pgdata":
		f.existing["up"] = true
	case line == "drbdadm down clw-shop-pgdata":
		f.existing["up"], f.existing["primary"] = false, false
	case strings.HasPrefix(line, "drbdadm primary"):
		f.existing["primary"] = true
	case line == "drbdadm secondary clw-shop-pgdata":
		f.existing["primary"] = false
	case name == "umount":
		f.existing["mounted"] = false
	case name == "blkid" && !f.existing["fs"]:
		return "", &command.ExitError{Code: 2}
	case name == "blkid":
		return "ext4\n", nil
	case name == "mkfs.ext4":
		f.existing["fs"] = true
	case name == "findmnt" && slices.Contains(args, "--list") && slices.Contains(args, "--raw"):
		// findmnt's own answer: the output formats exclude each other
		return "", &command.ExitError{Code: 1, Stderr: "findmnt: mutually exclusive arguments: --pairs --list --raw --verify"}
	case name == "findmnt" && slices.Contains(args, "--raw"):
		return f.mountTable(), nil
	case name == "findmnt" && !f.existing["mounted"]:
		return "", &command.ExitError{Code: 1}
	case name == "findmnt" && slices.Contains(args, "--source") && f.existing["other device"]:
		return "", &command.ExitError{Code: 1}
	case name == "mount":
		f.existing["mounted"] = true
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
	driver := Driver{
		VolumesDir: filepath.Join(dir, "volumes"), StateDir: filepath.Join(dir, "storage"),
		ResourceDir: filepath.Join(dir, "drbd.d"), Run: host.run,
	}
	host.mountPoint = driver.MountPoint(replicated())
	return driver
}

func replicated() Volume {
	return Volume{
		ID: "shop/pgdata", LV: "clw-shop-pgdata-g1", Pool: "vg-fast/thin", SizeBytes: 1 << 30,
		Resource: &Resource{Name: "clw-shop-pgdata", Minor: 3, Nodes: []Node{
			{Host: "node0", ID: 0, Disk: "/dev/vg-fast/clw-shop-pgdata-g1"},
		}},
	}
}

func plain() Volume {
	volume := replicated()
	volume.Resource = nil
	return volume
}

func TestANewCopyComesUpSecondaryAndUnmounted(t *testing.T) {
	host := &fakeHost{existing: map[string]bool{}}
	driver := newDriver(t, host)
	if err := driver.Ensure(context.Background(), replicated()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"lvcreate --quiet --thin vg-fast/thin --virtualsize 1073741824b --name clw-shop-pgdata-g1",
		"drbdadm create-md --force --max-peers=4 clw-shop-pgdata",
		"drbdadm adjust clw-shop-pgdata",
	} {
		if !host.ran(want) {
			t.Errorf("did not run %q; ran:\n%s", want, strings.Join(host.commands, "\n"))
		}
	}
	for _, never := range []string{"drbdadm primary", "mkfs.ext4", "mount ", "mkdir"} {
		if host.ran(never) {
			t.Errorf("ran %q:\n%s", never, strings.Join(host.commands, "\n"))
		}
	}
	if _, err := os.Stat(filepath.Join(driver.ResourceDir, "clw-shop-pgdata.res")); err != nil {
		t.Errorf("no resource file: %v", err)
	}
}

func TestANewVolumeIsForcedPrimaryFormattedAndMounted(t *testing.T) {
	host := &fakeHost{existing: map[string]bool{}}
	driver := newDriver(t, host)
	ctx := context.Background()
	if err := driver.Ensure(ctx, replicated()); err != nil {
		t.Fatal(err)
	}
	if err := driver.Promote(ctx, replicated(), true); err != nil {
		t.Fatal(err)
	}
	if err := driver.Mount(ctx, replicated(), true); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"drbdadm primary --force clw-shop-pgdata",
		"mkfs.ext4 -q /dev/drbd3",
		"mount -t ext4 /dev/drbd3 " + driver.MountPoint(replicated()),
		"mkdir -p " + filepath.Join(driver.MountPoint(replicated()), "data"),
	} {
		if !host.ran(want) {
			t.Errorf("did not run %q; ran:\n%s", want, strings.Join(host.commands, "\n"))
		}
	}
}

func TestAVolumeThatHoldsDataIsPromotedPlainly(t *testing.T) {
	host := &fakeHost{existing: map[string]bool{"lv": true, "up": true}}
	driver := newDriver(t, host)
	if err := driver.Promote(context.Background(), replicated(), false); err != nil {
		t.Fatal(err)
	}
	if host.ran("drbdadm primary --force") || !host.ran("drbdadm primary clw-shop-pgdata") {
		t.Errorf("ran:\n%s", strings.Join(host.commands, "\n"))
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
	for _, never := range []string{"lvcreate", "drbdadm create-md"} {
		if host.ran(never) {
			t.Errorf("ran %q again:\n%s", never, strings.Join(host.commands, "\n"))
		}
	}
}

func TestGivingUpACopyUnmountsAndDemotes(t *testing.T) {
	host := &fakeHost{existing: map[string]bool{"lv": true, "up": true, "primary": true, "fs": true, "mounted": true}}
	driver := newDriver(t, host)
	ctx := context.Background()
	if err := driver.Unmount(ctx, replicated()); err != nil {
		t.Fatal(err)
	}
	if err := driver.Demote(ctx, replicated()); err != nil {
		t.Fatal(err)
	}
	if !host.ran("umount "+driver.MountPoint(replicated())) || !host.ran("drbdadm secondary clw-shop-pgdata") {
		t.Errorf("ran:\n%s", strings.Join(host.commands, "\n"))
	}
	host.commands = nil
	if err := driver.Unmount(ctx, replicated()); err != nil || host.ran("umount") {
		t.Errorf("unmounted again (%v):\n%s", err, strings.Join(host.commands, "\n"))
	}
}

func TestRemovingAgainSkipsWhatIsGone(t *testing.T) {
	host := &fakeHost{existing: map[string]bool{}}
	driver := newDriver(t, host)
	ctx := context.Background()
	if err := driver.Ensure(ctx, replicated()); err != nil {
		t.Fatal(err)
	}
	if err := driver.Mount(ctx, replicated(), true); err != nil {
		t.Fatal(err)
	}
	if err := driver.Remove(ctx, replicated()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"umount", "drbdadm down clw-shop-pgdata", "lvremove --yes vg-fast/clw-shop-pgdata-g1"} {
		if !host.ran(want) {
			t.Errorf("did not run %q; ran:\n%s", want, strings.Join(host.commands, "\n"))
		}
	}
	// a crash before the copy's record was written again: removed once more
	host.commands = nil
	if err := driver.Remove(ctx, replicated()); err != nil {
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
		step  func(Driver) error
		never string
	}{
		{"blkid times out", "blkid", func(d Driver) error { return d.Mount(context.Background(), plain(), true) }, "mkfs.ext4"},
		{"lvs times out", "lvs", func(d Driver) error { return d.Ensure(context.Background(), plain()) }, "lvcreate"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			host := &fakeHost{existing: map[string]bool{"lv": true, "fs": false}}
			driver := newDriver(t, host)
			driver.Run = func(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error) {
				if name == test.tool {
					host.commands = append(host.commands, name)
					return "", errors.New("no answer within 1m0s")
				}
				return host.run(ctx, timeout, name, args...)
			}
			if err := test.step(driver); err == nil {
				t.Error("the step went on after a timeout")
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
	if err := driver.Ensure(context.Background(), plain()); err != nil {
		t.Fatal(err)
	}
	if err := driver.Mount(context.Background(), plain(), true); err != nil {
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
	err := driver.Mount(context.Background(), plain(), true)
	if err == nil || !strings.Contains(err.Error(), "clowder did not create LV clw-shop-pgdata-g1") {
		t.Errorf("want a refusal, got %v", err)
	}
	if host.ran("mkfs.ext4") {
		t.Error("an LV clowder did not make was formatted")
	}
}

func TestAVolumeThatHoldsDataIsNeverFormatted(t *testing.T) {
	// clowder made the LV, but the volume holds data: an empty device is
	// a copy without it, not a new one
	host := &fakeHost{existing: map[string]bool{}}
	driver := newDriver(t, host)
	if err := driver.Ensure(context.Background(), plain()); err != nil {
		t.Fatal(err)
	}
	err := driver.Mount(context.Background(), plain(), false)
	if err == nil || !strings.Contains(err.Error(), "this copy lacks the data") {
		t.Errorf("want a refusal, got %v", err)
	}
	if host.ran("mkfs.ext4") || host.ran("mount ") {
		t.Errorf("ran:\n%s", strings.Join(host.commands, "\n"))
	}
}

func TestAForeignFileSystemIsNotTouched(t *testing.T) {
	host := &fakeHost{existing: map[string]bool{"lv": true, "fs": true}}
	driver := newDriver(t, host)
	driver.Run = func(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error) {
		if name == "blkid" {
			return "xfs\n", nil
		}
		return host.run(ctx, timeout, name, args...)
	}
	if err := driver.Mount(context.Background(), plain(), true); err == nil || !strings.Contains(err.Error(), "holds xfs") {
		t.Errorf("want a refusal, got %v", err)
	}
}

func TestAnotherDeviceAtTheMountPointStopsTheMount(t *testing.T) {
	host := &fakeHost{existing: map[string]bool{"lv": true, "fs": true, "mounted": true, "other device": true}}
	driver := newDriver(t, host)
	if err := driver.Mount(context.Background(), plain(), false); err == nil || !strings.Contains(err.Error(), "another device") {
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
	if err := driver.Ensure(context.Background(), plain()); err != nil {
		t.Fatal(err)
	}
	if !host.ran("lvcreate") {
		t.Error("the copy's LV was taken as there")
	}
}

func TestObserveReadsTheHostOnceForAllCopies(t *testing.T) {
	other := plain()
	other.LV, other.Pool = "clw-blog-g1", "vg-bulk/thin"
	tests := []struct {
		name     string
		existing map[string]bool
		file     bool // the resource file is written
		want     Observed
	}{
		{"missing", map[string]bool{}, false, Observed{}},
		{"down after a boot", map[string]bool{"lv": true}, true, Observed{LV: true, ResourceFileCurrent: true}},
		{
			"secondary",
			map[string]bool{"lv": true, "up": true},
			true,
			Observed{LV: true, ResourceFileCurrent: true, Role: RoleSecondary},
		},
		{
			"primary and mounted",
			map[string]bool{"lv": true, "up": true, "primary": true, "mounted": true},
			true,
			Observed{LV: true, ResourceFileCurrent: true, Role: RolePrimary, Mounted: true},
		},
		{
			"another device mounted",
			map[string]bool{"lv": true, "up": true, "mounted": true, "other device": true},
			false,
			Observed{LV: true, Role: RoleSecondary, Mounted: true, OtherDevice: true},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			host := &fakeHost{existing: test.existing}
			driver := newDriver(t, host)
			if test.file {
				file := driver.resourceFile(replicated().Resource)
				if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(file, []byte(RenderResource(*replicated().Resource)), 0o600); err != nil {
					t.Fatal(err)
				}
				// as a successful adjust leaves it
				if err := driver.setMarker(replicated().Resource.Name, "adjusted", "default/db"); err != nil {
					t.Fatal(err)
				}
			}
			observed, err := driver.Observe(context.Background(), []Volume{replicated(), other})
			if err != nil {
				t.Fatal(err)
			}
			if observed[replicated().LV] != test.want {
				t.Errorf("observed %+v, want %+v", observed[replicated().LV], test.want)
			}
			if observed[other.LV] != (Observed{}) {
				t.Errorf("the other copy: %+v", observed[other.LV])
			}
			if len(host.commands) != 4 {
				t.Errorf("want lvs twice, drbdsetup and findmnt once; ran:\n%s", strings.Join(host.commands, "\n"))
			}
		})
	}
}

func TestObserveFailsWhenATableDoesNotRead(t *testing.T) {
	for _, tool := range []string{"lvs", "drbdsetup", "findmnt"} {
		t.Run(tool, func(t *testing.T) {
			host := &fakeHost{existing: map[string]bool{}}
			driver := newDriver(t, host)
			driver.Run = func(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error) {
				if name == tool {
					return "", errors.New("no answer within 10s")
				}
				return host.run(ctx, timeout, name, args...)
			}
			if _, err := driver.Observe(context.Background(), []Volume{replicated()}); err == nil {
				t.Error("a table that did not read was taken as empty")
			}
		})
	}
}

func TestTrimSkipsACopyThatIsNotMounted(t *testing.T) {
	host := &fakeHost{existing: map[string]bool{"lv": true, "fs": true}}
	driver := newDriver(t, host)
	if err := driver.Trim(context.Background(), plain()); err != nil || host.ran("fstrim") {
		t.Errorf("trimmed an empty mount point, the root file system (%v):\n%s", err, strings.Join(host.commands, "\n"))
	}
	host.existing["mounted"] = true
	if err := driver.Trim(context.Background(), plain()); err != nil || !host.ran("fstrim "+driver.MountPoint(plain())) {
		t.Errorf("no trim of the mounted copy (%v):\n%s", err, strings.Join(host.commands, "\n"))
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

func TestAResourceFileThatAdjustDidNotApplyIsNotCurrent(t *testing.T) {
	driver := newDriver(t, &fakeHost{})
	resource := replicated().Resource
	file := driver.resourceFile(resource)
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(RenderResource(*resource)), 0o600); err != nil {
		t.Fatal(err)
	}
	if current, err := driver.resourceFileCurrent(resource); err != nil || current {
		t.Errorf("current %v, %v before adjust applied it", current, err)
	}
	if err := driver.setMarker(resource.Name, "adjusted", "default/db"); err != nil {
		t.Fatal(err)
	}
	if current, err := driver.resourceFileCurrent(resource); err != nil || !current {
		t.Errorf("current %v, %v once adjust applied it", current, err)
	}
}

// A copy that became the tiebreaker on its host keeps the pool of its LV:
// Ensure removes the LV once adjust detached DRBD from it, and the marker
// of its metadata, so a disk that comes back gets new metadata.
func TestATiebreakerRemovesTheLVOfTheDiskItHad(t *testing.T) {
	host := &fakeHost{existing: map[string]bool{}}
	driver := newDriver(t, host)
	ctx := context.Background()
	if err := driver.Ensure(ctx, replicated()); err != nil {
		t.Fatal(err)
	}
	tiebreaker := replicated()
	tiebreaker.Diskless = true
	host.commands = nil
	if err := driver.Ensure(ctx, tiebreaker); err == nil || host.ran("lvremove") {
		t.Fatalf("removed the LV while DRBD still has it (%v):\n%s", err, strings.Join(host.commands, "\n"))
	}
	host.existing["diskless"] = true
	if err := driver.Ensure(ctx, tiebreaker); err != nil {
		t.Fatal(err)
	}
	if !host.ran("lvremove --yes vg-fast/clw-shop-pgdata-g1") {
		t.Errorf("did not remove the LV:\n%s", strings.Join(host.commands, "\n"))
	}
	for _, marker := range []string{"clw-shop-pgdata-g1.created", "clw-shop-pgdata.metadata"} {
		if _, err := os.Stat(filepath.Join(driver.StateDir, marker)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the marker %s is left: %v", marker, err)
		}
	}
}

func TestASplitBrainIsResolvedOnTheConnectionsThatStandAlone(t *testing.T) {
	host := &fakeHost{existing: map[string]bool{"lv": true, "up": true}}
	driver := newDriver(t, host)
	ctx := context.Background()
	volume := replicated()
	volume.Resource.Nodes = append(volume.Resource.Nodes, Node{Host: "node1", ID: 1, Disk: "/dev/vg-fast/clw-shop-pgdata-g1"})
	if err := driver.ResolveSplitBrain(ctx, volume, true); !errors.Is(err, ErrNoSplitBrain) {
		t.Errorf("without a peer that stands alone: %v", err)
	}
	host.existing["standalone"], host.existing["primary"] = true, true
	if err := driver.ResolveSplitBrain(ctx, volume, true); err == nil || host.ran("drbdadm connect") {
		t.Errorf("a primary discarded its data (%v):\n%s", err, strings.Join(host.commands, "\n"))
	}
	host.existing["primary"] = false
	if err := driver.ResolveSplitBrain(ctx, volume, true); err != nil {
		t.Fatal(err)
	}
	if !host.ran("drbdadm connect --discard-my-data clw-shop-pgdata:node1") {
		t.Errorf("ran:\n%s", strings.Join(host.commands, "\n"))
	}
	if err := driver.ResolveSplitBrain(ctx, volume, false); err != nil || !host.ran("drbdadm connect clw-shop-pgdata:node1") {
		t.Errorf("the kept copy (%v):\n%s", err, strings.Join(host.commands, "\n"))
	}
}
