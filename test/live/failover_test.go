//go:build live

package live

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/webcodingcat/clowder/internal/controller"
)

// failoverBudget is how long a stack may take to run again elsewhere once
// its host went: the detection, the power-off, the start, as README.md
// promises.
const failoverBudget = 3 * time.Minute

// backDeadline is how long a fenced host may take to be active again: the
// rest of its fence, the power-on, a boot, the agent's start and the resync
// of its copies.
const backDeadline = 15 * time.Minute

// failoverVolumeSize is the size of the scenarios' replicated volumes:
// small, so a host that comes back catches up in seconds.
const failoverVolumeSize = "256M"

// TestFailover fails one host three times within the hour, each time in
// another way, and checks that its stacks run elsewhere with their data,
// that it comes back, and that the third fence keeps it off until a user
// enables it. The steps count the fences of one host, so they run in order:
//   - the leader's host freezes (a kernel that hangs): fenced by the others;
//   - the same host, cut off from every network: fenced again;
//   - the hoster's API is down: nothing fails over until a user confirms the
//     fence, which is the third, so the host stays off (disabled);
//   - another host, cut off for a while and back: either back without a
//     fence, or fenced, never both.
func TestFailover(t *testing.T) {
	server, tofu := baseline(t)
	hosts := scenarioHosts(server, tofu)
	undrainJoined(t)
	cleanUpFaults(t)
	var target string // the host fenced three times: the leader at the start
	runSteps(t, []step{
		{"keeper has its copies on the hosts", func(t *testing.T) {
			createAndApply(t, "keeper", volumeDocument("keeper", "fast", failoverVolumeSize, 2)+simpleStack("keeper", "keeper"))
			createAndApply(t, "scratch", volumeDocument("scratch", "fast", failoverVolumeSize, 0)+simpleStack("scratch", "scratch"))
			waitRunning(t, "keeper", 1)
			waitRunning(t, "scratch", 1)
			volumeSteady(t, hosts, "keeper")
		}},
		{"the leader's host freezes, and its stacks run elsewhere", func(t *testing.T) {
			target = leaderName(t)
			moveTo(t, "keeper", target)
			volumeSteady(t, hosts, "keeper")
			writeProof(t, hosts[target], "keeper", "first")
			start := time.Now()
			labControl(t, "/vm/"+target+"/pause", nil)
			failsOver(t, hosts, "keeper", target, start, "first")
			waitBack(t, hosts, target, "keeper")
		}},
		{"the same host, cut off from every network, is fenced again", func(t *testing.T) {
			moveTo(t, "keeper", target)
			volumeSteady(t, hosts, "keeper")
			writeProof(t, hosts[target], "keeper", "second")
			isolate := map[string]string{"type": "isolate", "vm": target}
			start := time.Now()
			labControl(t, "/faults", isolate)
			waitPhase(t, target, "fenced", failoverBudget)
			// the fault outlives a power cycle; the host is off now
			labControl(t, "/faults/remove", isolate)
			failsOver(t, hosts, "keeper", target, start, "second")
			waitBack(t, hosts, target, "keeper")
		}},
		{"without the hoster's API nothing fails over, until a user confirms the fence", func(t *testing.T) {
			moveTo(t, "keeper", target)
			volumeSteady(t, hosts, "keeper")
			writeProof(t, hosts[target], "keeper", "third")
			labControl(t, "/api-fault", map[string]int{"status": http.StatusServiceUnavailable, "count": 100000})
			labControl(t, "/vm/"+target+"/pause", nil)
			waitPhase(t, target, "failed", failoverBudget)
			// the leader tries powerOffTries times, then gives the fence up
			stays(t, 2*time.Minute, target+" failed with keeper on it", func() error {
				phase, err := phaseOf(target)
				if err == nil && phase != "failed" && phase != "fencing" {
					return fmt.Errorf("%s is %s", target, phase)
				}
				if stacks, err := listStacks(); err == nil && stacks["keeper"].Host != target {
					return fmt.Errorf("keeper failed over to %s without a fence", stacks["keeper"].Host)
				}
				return nil
			})
			alertOpen(t, target+" cannot be fenced", true)
			labControl(t, "/api-fault", map[string]int{"status": 0, "count": 0})
			// the user makes sure the server is off, then says so
			powerOff(t, target)
			start := time.Now()
			mustClowder(t, "host", "fence-confirm", target, "--yes")
			failsOver(t, hosts, "keeper", target, start, "third")
		}},
		{"fenced three times within the hour, it stays off until a user enables it", func(t *testing.T) {
			waitPhase(t, target, "disabled", failoverBudget)
			if status := serverStatus(t, target); status != "off" {
				t.Errorf("the disabled host's server is %s", status)
			}
			alertOpen(t, target+" is disabled", true)
			mustClowder(t, "host", "enable", target)
			waitBack(t, hosts, target, "keeper")
			alertOpen(t, target+" is disabled", false)
			alertOpen(t, target+" cannot be fenced", false)
		}},
		{"a host that is back before the leader acts is fenced or not, never both", func(t *testing.T) {
			other := otherThan(slices.Sorted(maps.Keys(hosts)), target, leaderName(t))
			moveTo(t, "keeper", other)
			volumeSteady(t, hosts, "keeper")
			writeProof(t, hosts[other], "keeper", "fourth")
			boot := bootID(t, hosts[other])
			isolate := map[string]string{"type": "isolate", "vm": other}
			labControl(t, "/faults", isolate)
			seen := map[string]bool{}
			watchPhases(controller.FailAfter+15*time.Second, other, seen)
			labControl(t, "/faults/remove", isolate)
			eventually(t, backDeadline, other+" active", func() error {
				watchPhases(0, other, seen)
				phase, err := phaseOf(other)
				if err == nil && phase != "active" {
					err = fmt.Errorf("%s is %s", other, phase)
				}
				return err
			})
			rebooted := bootID(t, hosts[other]) != boot
			// the CLI may not reach the cluster while the host is cut off (it
			// is the first of its endpoints), so the short phases can pass unseen; recovering
			// comes only after a fence
			fenced := seen["fencing"] || seen["fenced"] || seen["recovering"]
			switch {
			case fenced && !rebooted:
				t.Errorf("%s was fenced but never powered off (phases %v)", other, seen)
			case !fenced && rebooted:
				t.Errorf("%s rebooted without a fence (phases %v)", other, seen)
			}
			t.Logf("%s went through %v; rebooted: %v", other, slices.Sorted(maps.Keys(seen)), rebooted)
			waitRunning(t, "keeper", 1)
			runsOnce(t, hosts, "keeper", "fourth")
		}},
		{"the hosts are as the baseline wants them again", func(t *testing.T) {
			removeStack(t, "keeper")
			removeStack(t, "scratch")
			drainJoined(t)
		}},
	})
}

// resyncVolumeSize is the size of TestNoFalseFailover's volume: its resync
// at the default resync rate (clusterspec.DefaultResyncRate, 50M/s: about 7
// minutes) outlasts the scenario's faults (about 3 minutes); a faster rate
// needs a bigger volume. The storage class bulk is 30 GB in the lab
// (bin/dev, live up) so that it fits: its pool is 90 % of the disk, and the
// placement keeps a reserve of 20 % free.
const resyncVolumeSize = "20G"

// resyncDeadline is how long the resync of resyncVolumeSize may take.
const resyncDeadline = 15 * time.Minute

// TestNoFalseFailover checks what must fail no host: a cut between the
// leader and one host only, and a pause of every host for 20 s, while a
// volume of 20 GB resyncs.
func TestNoFalseFailover(t *testing.T) {
	server, tofu := baseline(t)
	hosts := scenarioHosts(server, tofu)
	joined := undrainJoined(t)
	cleanUpFaults(t)
	var resyncStart time.Time
	runSteps(t, []step{
		{"a volume resyncs", func(t *testing.T) {
			createAndApply(t, "bulk", volumeDocument("bulk", "bulk", resyncVolumeSize, 2)+simpleStack("bulk", "bulk"))
			waitRunning(t, "bulk", 1)
			eventually(t, placementDeadline, "a second copy of bulk", func() error {
				if copies := len(volumeStatus(t, "bulk").Copies); copies < 2 {
					return fmt.Errorf("%d copies", copies)
				}
				return nil
			})
			resyncStart = time.Now()
		}},
		{"a cut between the leader and one host fails no host", func(t *testing.T) {
			leader := leaderName(t)
			cut := map[string]string{"type": "cut", "first": leader, "second": otherThan(joined, leader), "net": "all"}
			labControl(t, "/faults", cut)
			stays(t, controller.FailAfter+30*time.Second, "every host active", allActive)
			labControl(t, "/faults/remove", cut)
		}},
		{"a pause of every host for 20 s restarts and moves nothing", func(t *testing.T) {
			before := map[string]string{}
			for name, server := range hosts {
				before[name] = bootID(t, server)
			}
			bulkHost := stackInfo(t, "bulk").Host
			container := stackContainer(t, hosts[bulkHost], "bulk")
			for name := range hosts {
				labControl(t, "/vm/"+name+"/pause", nil)
			}
			time.Sleep(20 * time.Second)
			for name := range hosts {
				labControl(t, "/vm/"+name+"/cont", nil)
			}
			stays(t, controller.FailAfter+30*time.Second, "every host active", allActive)
			for name, server := range hosts {
				if boot := bootID(t, server); boot != before[name] {
					t.Errorf("%s rebooted", name)
				}
			}
			if now := stackContainer(t, hosts[bulkHost], "bulk"); now != container {
				t.Errorf("bulk's container on %s changed: %s, then %s", bulkHost, container, now)
			}
			// else the faults did not meet a resync, which is what this checks
			if status := volumeStatus(t, "bulk").Status; status == "ok" {
				t.Errorf("bulk synced before the faults ended; it needs to be bigger than %s", resyncVolumeSize)
			}
		}},
		{"the resync ends without a host failed", func(t *testing.T) {
			waitSynced(t, "bulk", resyncDeadline)
			t.Logf("bulk's second copy of %s synced in %s", resyncVolumeSize, time.Since(resyncStart).Round(time.Second))
			if err := allActive(); err != nil {
				t.Error(err)
			}
		}},
		{"the hosts are as the baseline wants them again", func(t *testing.T) {
			removeStack(t, "bulk")
			drainJoined(t)
		}},
	})
}

// TestCrashingAgent checks that an agent that crashes at once, such as a
// broken release, leads to no reboot loop: the host is fenced once, powered
// on, and stays fenced while its agent does not run.
func TestCrashingAgent(t *testing.T) {
	server, tofu := baseline(t)
	hosts := scenarioHosts(server, tofu)
	joined := undrainJoined(t)
	cleanUpFaults(t)
	victim := otherThan(joined, leaderName(t))
	const dropIn = "/etc/systemd/system/clowderd.service.d/crash.conf"
	runSteps(t, []step{
		{"an agent that crashes at once gets its host fenced", func(t *testing.T) {
			// a stack on it that another host can take: else no fence is needed
			createAndApply(t, "crasher", simpleStack("crasher", ""))
			waitRunning(t, "crasher", 1)
			moveTo(t, "crasher", victim)
			boot := bootID(t, hosts[victim])
			hosts[victim].must(t, "mkdir -p "+dropIn[:strings.LastIndex(dropIn, "/")]+" && printf '[Service]\\nExecStart=\\nExecStart=/bin/false\\n' > "+
				dropIn+" && systemctl daemon-reload && (systemctl restart clowderd || true)")
			waitPhase(t, victim, "fenced", failoverBudget)
			eventually(t, backDeadline, victim+" powered on again", func() error {
				id, err := hosts[victim].run("cat /proc/sys/kernel/random/boot_id")
				if err == nil && id == boot {
					err = errors.New("still the same boot")
				}
				return err
			})
		}},
		{"it stays fenced, and the host does not boot again", func(t *testing.T) {
			boot := bootID(t, hosts[victim])
			stays(t, 2*time.Minute, victim+" fenced", func() error {
				if phase, err := phaseOf(victim); err == nil && phase != "fenced" {
					return fmt.Errorf("%s is %s", victim, phase)
				}
				return nil
			})
			if now := bootID(t, hosts[victim]); now != boot {
				t.Errorf("%s booted again", victim)
			}
		}},
		{"a working agent brings it back", func(t *testing.T) {
			hosts[victim].must(t, "rm "+dropIn+" && systemctl daemon-reload && systemctl reset-failed clowderd && systemctl start clowderd")
			waitPhase(t, victim, "active", backDeadline)
		}},
		{"the hosts are as the baseline wants them again", func(t *testing.T) {
			removeStack(t, "crasher")
			drainJoined(t)
		}},
	})
}

// TestMoveInterrupted fails the host a stack moves to while its copy
// syncs: the move ends at once, and the stack runs once, where it was.
func TestMoveInterrupted(t *testing.T) {
	server, tofu := baseline(t)
	hosts := scenarioHosts(server, tofu)
	joined := undrainJoined(t)
	cleanUpFaults(t)
	var from, to string
	runSteps(t, []step{
		{"a stack starts to move", func(t *testing.T) {
			createAndApply(t, "mover", volumeDocument("mover", "fast", placedVolumeSize, 1)+simpleStack("mover", "mover"))
			waitRunning(t, "mover", 1)
			from = stackInfo(t, "mover").Host
			writeProof(t, hosts[from], "mover", "here")
			// not the first host: the CLI asks it first, and a pause would cut it off
			to = otherThan(joined, from)
			mustClowder(t, "stack", "move", "mover", to)
			eventually(t, placementDeadline, "a copy of mover on "+to, func() error {
				for _, volumeCopy := range volumeStatus(t, "mover").Copies {
					if volumeCopy.Host == to {
						return nil
					}
				}
				return fmt.Errorf("copies %+v", volumeStatus(t, "mover").Copies)
			})
			// the new copy came with a tiebreaker, in one command: once the old
			// host sees it, its quorum does not hang on the new host
			eventually(t, placementDeadline, from+" sees the tiebreaker of mover", func() error {
				for _, volumeCopy := range volumeStatus(t, "mover").Copies {
					if volumeCopy.Tiebreaker {
						seen := hosts[from].must(t, "drbdsetup status "+volumeLV(t, "mover"))
						if !strings.Contains(seen, volumeCopy.Host+" role:") {
							return fmt.Errorf("not connected:\n%s", seen)
						}
						return nil
					}
				}
				return fmt.Errorf("no tiebreaker: %+v", volumeStatus(t, "mover").Copies)
			})
		}},
		{"its new host fails, and it runs where it was", func(t *testing.T) {
			labControl(t, "/vm/"+to+"/pause", nil)
			// the stack keeps writing: its copy has its quorum with the tiebreaker
			hosts[from].must(t, "timeout 30 docker exec "+stackContainer(t, hosts[from], "mover")+
				" sh -c 'echo there > /data/proof && sync'")
			eventually(t, failoverBudget, "the move of mover over", func() error {
				stacks, err := listStacks()
				if err != nil {
					return err
				}
				if stack := stacks["mover"]; stack.Host != from || stack.Status != "running" {
					return fmt.Errorf("%s on %s", stack.Status, stack.Host)
				}
				// the copy the move left goes once the move ended and freed
				// the volume
				if copies := volumeStatus(t, "mover").Copies; len(copies) != 1 || copies[0].Host != from {
					return fmt.Errorf("copies %+v", copies)
				}
				return nil
			})
			runsOnce(t, hosts, "mover", "there")
			// it had no stack to take over, so it is not fenced: it stays
			// failed until it is heard again
			if phase, err := phaseOf(to); err != nil || phase != "failed" {
				t.Errorf("%s is %s (%v), want failed", to, phase, err)
			}
			labControl(t, "/vm/"+to+"/cont", nil)
			waitPhase(t, to, "active", backDeadline)
		}},
		{"the hosts are as the baseline wants them again", func(t *testing.T) {
			removeStack(t, "mover")
			drainJoined(t)
		}},
	})
}

// scenarioHosts are every host of the cluster by name, the first one
// included.
func scenarioHosts(server host, tofu outputs) map[string]host {
	hosts := map[string]host{firstHost: server}
	for name, module := range tofu.Hosts.Value {
		hosts[name] = host{address: module.PublicAddress}
	}
	return hosts
}

// undrainJoined lets the joined hosts, drained at the baseline, take stacks
// and returns their names.
func undrainJoined(t *testing.T) []string {
	t.Helper()
	var joined []string
	for _, found := range clusterHosts(t) {
		if found.Phase == "draining" {
			joined = append(joined, found.Name)
			mustClowder(t, "host", "undrain", found.Name)
		}
	}
	return joined
}

// drainJoined drains every host but the first, as the baseline wants it.
func drainJoined(t *testing.T) {
	t.Helper()
	for _, found := range clusterHosts(t) {
		if found.ID != 0 && found.Phase == "active" {
			mustClowder(t, "host", "drain", found.Name)
		}
	}
}

// otherThan is a host of names that is none of the others.
func otherThan(names []string, others ...string) string {
	for _, name := range names {
		if !slices.Contains(others, name) {
			return name
		}
	}
	return ""
}

// leaderPattern finds the leader in clowder cluster status.
var leaderPattern = regexp.MustCompile(`leader ([^ ,]+),`)

// leaderName is the host that leads now.
func leaderName(t *testing.T) string {
	t.Helper()
	var leader string
	eventually(t, time.Minute, "a leader", func() error {
		status, err := clowder("cluster", "status")
		if err != nil {
			return err
		}
		match := leaderPattern.FindStringSubmatch(status)
		if match == nil || match[1] == "none" {
			return fmt.Errorf("no leader:\n%s", status)
		}
		leader = match[1]
		return nil
	})
	return leader
}

// labControl sends a command to HLab's lab control (HLAB_CONTROL): a fault,
// a VM paused or continued, the API's failures.
func labControl(t *testing.T, path string, body any) {
	t.Helper()
	encoded := []byte("{}")
	if body != nil {
		var err error
		if encoded, err = json.Marshal(body); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, os.Getenv("HLAB_CONTROL")+path, bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }() // only the status matters
	if response.StatusCode >= 300 {
		t.Fatalf("lab control %s: HTTP %d", path, response.StatusCode)
	}
}

// cleanUpFaults clears the lab's faults when the scenario ends, also after a
// failure, so the next one finds its baseline; a paused VM goes on.
func cleanUpFaults(t *testing.T) {
	t.Cleanup(func() {
		labControl(t, "/faults/clear", nil)
		for _, name := range []string{firstHost, firstJoining, secondJoining, thirdJoining, replacement} {
			// a VM that is not paused, or not there, answers with an error
			// that changes nothing
			request, err := http.NewRequest(http.MethodPost, os.Getenv("HLAB_CONTROL")+"/vm/"+name+"/cont", nil)
			if err == nil {
				if response, err := http.DefaultClient.Do(request); err == nil {
					_ = response.Body.Close()
				}
			}
		}
	})
}

// phaseOf is a host's phase in clowder host list.
func phaseOf(name string) (string, error) {
	hosts, err := listHosts()
	if err != nil {
		return "", err
	}
	if found, ok := findHost(hosts, name); ok {
		return found.Phase, nil
	}
	return "", fmt.Errorf("no host %s", name)
}

// waitPhase waits until a host is in a phase.
func waitPhase(t *testing.T, name, phase string, deadline time.Duration) {
	t.Helper()
	eventually(t, deadline, name+" "+phase, func() error {
		got, err := phaseOf(name)
		if err == nil && got != phase {
			err = fmt.Errorf("it is %s", got)
		}
		return err
	})
}

// watchPhases records the phases a host goes through for a while (0: one
// look).
func watchPhases(during time.Duration, name string, seen map[string]bool) {
	end := time.Now().Add(during)
	for {
		if phase, err := phaseOf(name); err == nil {
			seen[phase] = true
		}
		if !time.Now().Before(end) {
			return
		}
		time.Sleep(2 * time.Second)
	}
}

// stays checks for a while that something holds the whole time.
func stays(t *testing.T, during time.Duration, what string, check func() error) {
	t.Helper()
	end := time.Now().Add(during)
	for time.Now().Before(end) {
		if err := check(); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		time.Sleep(2 * time.Second)
	}
}

// alertOpen waits until clowder cluster status lists an alert with the
// subject as open, or (open false) no longer lists it.
func alertOpen(t *testing.T, subject string, open bool) {
	t.Helper()
	what := "the alert " + subject + " resolved"
	if open {
		what = "the alert " + subject + " open"
	}
	eventually(t, time.Minute, what, func() error {
		status, err := clowder("cluster", "status")
		if err == nil && strings.Contains(status, subject) != open {
			err = fmt.Errorf("cluster status:\n%s", status)
		}
		return err
	})
}

// allActive says why not every host is active, if one is not; a failed
// listing (a change of leader) passes.
func allActive() error {
	hosts, err := listHosts()
	if err != nil {
		return nil
	}
	for _, found := range hosts {
		if found.Phase != "active" && found.Phase != "removed" {
			return fmt.Errorf("%s is %s", found.Name, found.Phase)
		}
	}
	return nil
}

// moveTo moves a stack to a host unless it runs there.
func moveTo(t *testing.T, stack, to string) {
	t.Helper()
	if stackInfo(t, stack).Host == to {
		return
	}
	mustClowder(t, "stack", "move", stack, to)
	eventually(t, placementDeadline, stack+" running on "+to, func() error {
		if listed := stackInfo(t, stack); listed.Status != "running" || listed.Host != to {
			return fmt.Errorf("%s on %s (%s)", listed.Status, listed.Host, listed.WaitReason)
		}
		return nil
	})
}

// waitSynced waits until every copy of a volume is synced, at most until
// the deadline.
func waitSynced(t *testing.T, volume string, deadline time.Duration) {
	t.Helper()
	eventually(t, deadline, "volume "+volume+" synced", func() error {
		if status := volumeStatus(t, volume); status.Status != "ok" {
			return fmt.Errorf("%s, copies %+v", status.Status, status.Copies)
		}
		return nil
	})
}

// failsOver waits until a stack runs on another host than the one that
// went, within failoverBudget of start, with its data, and once.
func failsOver(t *testing.T, hosts map[string]host, stack, gone string, start time.Time, word string) {
	t.Helper()
	eventually(t, 2*failoverBudget, stack+" running off "+gone, func() error {
		stacks, err := listStacks()
		if err != nil {
			return err
		}
		if listed := stacks[stack]; listed.Status != "running" || listed.Host == gone {
			return fmt.Errorf("%s on %s (%s)", listed.Status, listed.Host, listed.WaitReason)
		}
		return nil
	})
	took := time.Since(start)
	t.Logf("%s ran again on %s %s after %s went", stack, stackInfo(t, stack).Host, took.Round(time.Second), gone)
	if took > failoverBudget {
		t.Errorf("the failover took %s, longer than %s", took.Round(time.Second), failoverBudget)
	}
	runsOnce(t, hosts, stack, word)
}

// waitBack waits until a fenced host is active again, and checks that the
// stack runs once, with one primary of its volume.
func waitBack(t *testing.T, hosts map[string]host, name, stack string) {
	t.Helper()
	waitPhase(t, name, "active", backDeadline)
	runsOnce(t, hosts, stack, "")
}

// runsOnce checks that the stack's container runs on its host alone, that
// only its host has its volume primary, and that it holds the word (""
// skips it). A host that does not answer is down and runs nothing.
func runsOnce(t *testing.T, hosts map[string]host, stack, word string) {
	t.Helper()
	on := stackInfo(t, stack).Host
	lv := volumeLV(t, stack)
	for name, server := range hosts {
		if _, err := server.run("true"); err != nil {
			continue
		}
		running := stackContainer(t, server, stack) != ""
		// the first line is this host's own role; the lines after it are its peers'
		primary := strings.Contains(server.must(t, "drbdsetup status "+lv+" 2>/dev/null | head -n 1"), "role:Primary")
		switch {
		case name == on && !running:
			t.Errorf("%s does not run on its host %s", stack, name)
		case name != on && running:
			t.Errorf("%s runs on %s too", stack, name)
		case name != on && primary:
			t.Errorf("%s's volume is primary on %s too", stack, name)
		}
	}
	if word != "" {
		checkProof(t, hosts[on], stack, word)
	}
}

// bootID tells the host's boot apart from the next one.
func bootID(t *testing.T, server host) string {
	t.Helper()
	return server.must(t, "cat /proc/sys/kernel/random/boot_id")
}

// serverOf is the lab's server of a host, by name.
func serverOf(t *testing.T, name string) (id int64, status string) {
	t.Helper()
	var answer struct {
		Servers []struct {
			ID     int64  `json:"id"`
			Status string `json:"status"`
		} `json:"servers"`
	}
	cloud(t, http.MethodGet, "/servers?name="+name, nil, &answer)
	if len(answer.Servers) != 1 {
		t.Fatalf("the lab has %d servers named %s", len(answer.Servers), name)
	}
	return answer.Servers[0].ID, answer.Servers[0].Status
}

// serverStatus is the status of the host's server at the hoster: running,
// off, ...
func serverStatus(t *testing.T, name string) string {
	t.Helper()
	_, status := serverOf(t, name)
	return status
}

// powerOff powers the host's server off, as a user who checks that it is.
func powerOff(t *testing.T, name string) {
	t.Helper()
	id, _ := serverOf(t, name)
	cloud(t, http.MethodPost, fmt.Sprintf("/servers/%d/actions/poweroff", id), nil, nil)
	eventually(t, time.Minute, name+"'s server off", func() error {
		if status := serverStatus(t, name); status != "off" {
			return fmt.Errorf("it is %s", status)
		}
		return nil
	})
}

// volumeSteady waits until a volume of two replicas has both copies synced and
// its tiebreaker, and each host with a copy sees the others connected: a
// scenario that fails a host starts from there.
func volumeSteady(t *testing.T, hosts map[string]host, volume string) {
	t.Helper()
	eventually(t, placementDeadline, "volume "+volume+" with two synced copies and a tiebreaker", func() error {
		status := volumeStatus(t, volume)
		tiebreakers := 0
		for _, volumeCopy := range status.Copies {
			if volumeCopy.Tiebreaker {
				tiebreakers++
			}
		}
		if status.Status != "ok" || len(status.Copies) != 3 || tiebreakers != 1 {
			return fmt.Errorf("%s, copies %+v", status.Status, status.Copies)
		}
		// the state is ahead of DRBD: each host with a copy must see the
		// others connected (a connected peer shows its role)
		lv := volumeLV(t, volume)
		for _, volumeCopy := range status.Copies {
			seen, err := hosts[volumeCopy.Host].run("drbdsetup status " + lv)
			if err != nil {
				return err
			}
			for _, peer := range status.Copies {
				if peer.Host != volumeCopy.Host && !strings.Contains(seen, peer.Host+" role:") {
					return fmt.Errorf("%s does not see %s connected:\n%s", volumeCopy.Host, peer.Host, seen)
				}
			}
		}
		return nil
	})
}
