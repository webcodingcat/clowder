//go:build live

package live

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/webcodingcat/clowder/internal/api"
	"github.com/webcodingcat/clowder/internal/resource"
	"github.com/webcodingcat/clowder/internal/state"
)

// firstHost is the module's first host, which TestSetup bootstrapped.
const firstHost = "clowder0"

// placementDeadline is how long a copy may take to come and sync, or a
// stack to move: a resync of placedVolumeSize at the resync rate and the
// passes around it.
const placementDeadline = 10 * time.Minute

// placedVolumeSize is the size of the scenario's replicated volume: big
// enough that its first resync takes a while at the resync rate.
const placedVolumeSize = "2G"

// TestPlacement places volume copies and moves stacks over the three hosts:
// a volume gets its second copy and a tiebreaker once the joined hosts take
// stacks; a stack moves to the host of its copy, to the tiebreaker's, and
// off a host that drains; a volume of one replica moves with its stack; and
// the data stays with them.
func TestPlacement(t *testing.T) {
	server, tofu := baseline(t)
	hosts := map[string]host{firstHost: server}
	for name, module := range tofu.Hosts.Value {
		hosts[name] = host{address: module.PublicAddress}
	}
	// the joined hosts, drained at the baseline; TestReplace changes which
	var joined []string
	for _, found := range clusterHosts(t) {
		if found.Phase == "draining" {
			joined = append(joined, found.Name)
		}
	}
	var copyHost, tiebreakerHost string
	runSteps(t, []step{
		{"a volume on one host waits for its copy", func(t *testing.T) {
			createAndApply(t, "keeper", volumeDocument("keeper", "fast", placedVolumeSize, 2)+simpleStack("keeper", "keeper"))
			waitRunning(t, "keeper", 1)
			if status := volumeStatus(t, "keeper"); status.Status != "degraded" {
				t.Errorf("volume keeper with one copy of two is %q", status.Status)
			}
			writeProof(t, hosts[firstHost], "keeper", "first")
		}},
		{"it gets its copy and a tiebreaker once the hosts take stacks", func(t *testing.T) {
			start := time.Now()
			for _, name := range joined {
				mustClowder(t, "host", "undrain", name)
			}
			eventually(t, placementDeadline, "volume keeper synced with a tiebreaker", func() error {
				status := volumeStatus(t, "keeper")
				if status.Status != "ok" || len(status.Copies) != 3 {
					return fmt.Errorf("%s, copies %+v", status.Status, status.Copies)
				}
				return nil
			})
			t.Logf("the second copy of %s came and synced in %s", placedVolumeSize, time.Since(start).Round(time.Second))
			for _, volumeCopy := range volumeStatus(t, "keeper").Copies {
				switch {
				case volumeCopy.Tiebreaker:
					tiebreakerHost = volumeCopy.Host
				case volumeCopy.Host != firstHost:
					copyHost = volumeCopy.Host
				}
			}
			lv := volumeLV(t, "keeper")
			// the copy without its stack is secondary and unmounted, the
			// tiebreaker has no disk
			copyStatus := hosts[copyHost].must(t, "drbdsetup status "+lv)
			if !strings.Contains(copyStatus, "role:Secondary") || !strings.Contains(copyStatus, "disk:UpToDate") {
				t.Errorf("the copy on %s:\n%s", copyHost, copyStatus)
			}
			if mounted, _ := hosts[copyHost].run("findmnt --noheadings /var/lib/clowder/volumes/" + lv); mounted != "" {
				t.Errorf("the copy on %s is mounted: %s", copyHost, mounted)
			}
			if status := hosts[tiebreakerHost].must(t, "drbdsetup status "+lv); !strings.Contains(status, "disk:Diskless") {
				t.Errorf("the tiebreaker on %s:\n%s", tiebreakerHost, status)
			}
		}},
		{"the copy's host has the stack's image before it needs it", func(t *testing.T) {
			eventually(t, stackDeadline, "the image on "+copyHost, func() error {
				_, err := hosts[copyHost].run("docker image inspect " + publicImage)
				return err
			})
		}},
		{"the stack moves to the host of its copy", func(t *testing.T) {
			moveAndCheck(t, hosts, "keeper", firstHost, copyHost, "first")
			writeProof(t, hosts[copyHost], "keeper", "second")
		}},
		{"it moves to the tiebreaker's host, which gets a disk", func(t *testing.T) {
			moveAndCheck(t, hosts, "keeper", copyHost, tiebreakerHost, "second")
			eventually(t, placementDeadline, "two copies and a new tiebreaker", func() error {
				status := volumeStatus(t, "keeper")
				disks := slices.DeleteFunc(slices.Clone(status.Copies), func(c api.VolumeCopy) bool { return c.Tiebreaker })
				if status.Status != "ok" || len(disks) != 2 || len(status.Copies) != 3 ||
					!slices.ContainsFunc(disks, func(c api.VolumeCopy) bool { return c.Host == tiebreakerHost }) {
					return fmt.Errorf("%s, copies %+v", status.Status, status.Copies)
				}
				return nil
			})
		}},
		{"a drain moves the stack off and keeps the copies", func(t *testing.T) {
			before := volumeStatus(t, "keeper").Copies
			mustClowder(t, "host", "drain", tiebreakerHost)
			eventually(t, placementDeadline, "keeper off "+tiebreakerHost, func() error {
				if stack := stackInfo(t, "keeper"); stack.Status != "running" || stack.Host == tiebreakerHost {
					return fmt.Errorf("%s on %s", stack.Status, stack.Host)
				}
				return nil
			})
			landed := stackInfo(t, "keeper").Host
			checkProof(t, hosts[landed], "keeper", "second")
			if after := volumeStatus(t, "keeper").Copies; !sameHosts(before, after) {
				t.Errorf("the copies changed with the drain: %+v, then %+v", before, after)
			}
			mustClowder(t, "host", "undrain", tiebreakerHost)
		}},
		{"a volume of one replica moves with a copy of its own", func(t *testing.T) {
			createAndApply(t, "single", volumeDocument("single", "fast", "1G", 1)+simpleStack("single", "single"))
			waitRunning(t, "single", 1)
			from := stackInfo(t, "single").Host
			writeProof(t, hosts[from], "single", "alone")
			to := firstHost
			if from == firstHost {
				to = joined[0]
			}
			moveAndCheck(t, hosts, "single", from, to, "alone")
			eventually(t, placementDeadline, "one copy of single, on "+to, func() error {
				status := volumeStatus(t, "single")
				if len(status.Copies) != 1 || status.Copies[0].Host != to {
					return fmt.Errorf("copies %+v", status.Copies)
				}
				return nil
			})
		}},
		{"the hosts are as the baseline wants them again", func(t *testing.T) {
			removeStack(t, "keeper")
			removeStack(t, "single")
			for _, name := range joined {
				mustClowder(t, "host", "drain", name)
			}
		}},
	})
}

// volumeStatus is a volume of the namespace default as get lists it.
func volumeStatus(t *testing.T, name string) api.VolumeStatus {
	t.Helper()
	var object resource.Object
	if err := json.Unmarshal([]byte(mustClowder(t, "get", "volume", name, "-o", "json")), &object); err != nil {
		t.Fatalf("read volume %s: %v", name, err)
	}
	var status api.VolumeStatus
	if err := json.Unmarshal(object.Status, &status); err != nil {
		t.Fatalf("read the status of volume %s: %v", name, err)
	}
	return status
}

// stackContainer is the ID of the container of a stack's service app on a
// host; empty if none runs there.
func stackContainer(t *testing.T, server host, stack string) string {
	t.Helper()
	return server.must(t, "docker ps -q --no-trunc --filter label=com.docker.compose.project="+project(stack)+
		" --filter label=com.docker.compose.service=app")
}

// writeProof writes a word into the stack's volume, through its container.
func writeProof(t *testing.T, server host, stack, word string) {
	t.Helper()
	server.must(t, "docker exec "+stackContainer(t, server, stack)+" sh -c 'echo "+word+" > /data/proof && sync'")
}

// checkProof reads the word back from the stack's volume.
func checkProof(t *testing.T, server host, stack, word string) {
	t.Helper()
	if got := server.must(t, "docker exec "+stackContainer(t, server, stack)+" cat /data/proof"); got != word {
		t.Errorf("the volume of %s holds %q, want %q", stack, got, word)
	}
}

// moveAndCheck moves a stack and checks that it runs once, on the new
// host, with its data.
func moveAndCheck(t *testing.T, hosts map[string]host, stack, from, to, word string) {
	t.Helper()
	start := time.Now()
	mustClowder(t, "stack", "move", stack, to)
	eventually(t, placementDeadline, stack+" running on "+to, func() error {
		if listed := stackInfo(t, stack); listed.Status != "running" || listed.Host != to {
			return fmt.Errorf("%s on %s (%s)", listed.Status, listed.Host, listed.WaitReason)
		}
		return nil
	})
	took := time.Since(start)
	t.Logf("%s moved from %s to %s in %s", stack, from, to, took.Round(time.Second))
	// a move that ran into a step's deadline ends failed, even if the stack
	// runs where it should
	if took > state.StepTime {
		t.Errorf("the move of %s took %s, longer than a step may (%s)", stack, took.Round(time.Second), state.StepTime)
	}
	checkProof(t, hosts[to], stack, word)
	for name, other := range hosts {
		if name != to && stackContainer(t, other, stack) != "" {
			t.Errorf("%s runs on %s too", stack, name)
		}
	}
}

// sameHosts tells whether two lists of copies are on the same hosts, in
// the same roles.
func sameHosts(a, b []api.VolumeCopy) bool {
	key := func(copies []api.VolumeCopy) []string {
		var keys []string
		for _, volumeCopy := range copies {
			keys = append(keys, fmt.Sprintf("%s/%t", volumeCopy.Host, volumeCopy.Tiebreaker))
		}
		slices.Sort(keys)
		return keys
	}
	return slices.Equal(key(a), key(b))
}
