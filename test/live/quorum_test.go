//go:build live

package live

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/webcodingcat/clowder/internal/api"
	"github.com/webcodingcat/clowder/internal/clusterspec"
)

// TestQuorum checks DRBD's quorum where a volume's copies change, and an
// agent's own alert without Raft's quorum:
//   - the host a stack just moved to fails before anything settled: its
//     copy and tiebreaker changed in one command, so the copy elsewhere
//     keeps its quorum and the stack fails over;
//   - two hosts of three go: nothing is fenced, and the one left alerts by
//     itself, and gives the all-clear when they are back.
//
// A split brain has no scenario: with DRBD's quorum neither a fault of the
// lab nor a hand on the hosts makes one (a copy promoted apart from newer
// data has no quorum, and its writes hang), so its detection, alert and
// resolve-splitbrain are unit tests.
func TestQuorum(t *testing.T) {
	server, tofu := baseline(t, threeHosts)
	hosts := scenarioHosts(server, tofu)
	joined := undrainJoined(t)
	cleanUpFaults(t)
	runSteps(t, []step{
		{"the host a stack just moved to fails, and the stack fails over", func(t *testing.T) {
			createAndApply(t, "shifter", volumeDocument("shifter", "fast", failoverVolumeSize, 2)+simpleStack("shifter", "shifter"))
			waitRunning(t, "shifter", 1)
			volumeSteady(t, hosts, "shifter")
			// not the first host: the CLI asks it first, and a pause would cut it off
			target := otherThan(joined, stackInfo(t, "shifter").Host)
			moveTo(t, "shifter", target)
			writeProof(t, hosts[target], "shifter", "moved")
			start := time.Now()
			labControl(t, "/vm/"+target+"/pause", nil)
			failsOver(t, hosts, "shifter", target, start, "moved")
			waitBack(t, hosts, target, "shifter")
			removeStack(t, "shifter")
		}},
		{"two hosts of three go: nothing is fenced, and the one left alerts by itself", func(t *testing.T) {
			twoHostsGo(t, joined)
		}},
		{"the hosts are as the baseline wants them again", func(t *testing.T) {
			drainJoined(t)
		}},
	})
}

// twoHostsGo pauses both joined hosts with an email channel set up: the
// first host knows no leader, fences nothing and mails no-leader, and the
// all-clear once they are back.
func twoHostsGo(t *testing.T, joined []string) {
	channel := emailChannel(t)
	first := readOutputs(t).PublicAddress.Value
	before := configGenerationOn(t, first)
	changeConfig(t, func(config *clusterspec.Config) {
		config.Alerts = &clusterspec.Alerts{Channels: []clusterspec.AlertChannel{channel}}
	})
	// the first host alerts from its own state: it must have the channel
	// before the others go, or it learns of it only once they are back
	eventually(t, time.Minute, "the first host has the email channel", func() error {
		if generation := configGenerationOn(t, first); generation <= before {
			return fmt.Errorf("the first host has the configuration's generation %d", generation)
		}
		return nil
	})
	subject := "no quorum as seen from " + firstHost
	for _, name := range joined {
		labControl(t, "/vm/"+name+"/pause", nil)
	}
	eventually(t, noLeaderDeadline, "the mail "+subject, func() error {
		if subjects := mailSubjects(t); !strings.Contains(subjects, "critical: "+subject) {
			return fmt.Errorf("the mail has:\n%s", subjects)
		}
		return nil
	})
	for _, name := range joined {
		labControl(t, "/vm/"+name+"/cont", nil)
	}
	eventually(t, 2*time.Minute, "the all-clear "+subject, func() error {
		if subjects := mailSubjects(t); !strings.Contains(subjects, "resolved: "+subject) {
			return fmt.Errorf("the mail has:\n%s", subjects)
		}
		return nil
	})
	for _, name := range joined {
		waitPhase(t, name, "active", backDeadline)
		// paused, not fenced: the same boot
		if phase, err := phaseOf(name); err != nil || phase != "active" {
			t.Errorf("%s is %s (%v)", name, phase, err)
		}
	}
	changeConfig(t, func(config *clusterspec.Config) { config.Alerts = nil })
}

// configGenerationOn is the generation of the ClusterConfiguration that one
// host has applied: the CLI's status asks whichever host answers first.
func configGenerationOn(t *testing.T, address string) int64 {
	t.Helper()
	var status api.Status
	if err := namedClient(t, credentialsFile(), address).Get(context.Background(), "/v1/status", &status); err != nil {
		t.Fatal(err)
	}
	return status.Generation
}

// noLeaderDeadline is how long the agent left may take to mail no-leader:
// the two minutes it knows no leader first (controller.noLeaderFor), its
// round, Raft's timeouts and the mail.
const noLeaderDeadline = 4 * time.Minute
