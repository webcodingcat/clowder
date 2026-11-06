//go:build live

package live

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/webcodingcat/clowder/internal/clientconfig"
	"github.com/webcodingcat/clowder/internal/clusterspec"
)

// TestSingleHost runs a cluster of one host with every role, as a cluster
// of its own: it leads alone without an alert, runs stacks with a volume of
// two replicas on its one copy, serves through the Floating IP, and keeps
// its stacks through an agent restart and a reboot, with no other host to
// fence it or to take over.
func TestSingleHost(t *testing.T) {
	server, tofu := baseline(t, singleHost)
	runSteps(t, []step{
		{"the host alone is the cluster, without an alert", func(t *testing.T) { aloneWithoutAlert(t, tofu) }},
		{"stacks run, a volume of two replicas on its one copy", func(t *testing.T) { runAlone(t, server) }},
		{"HTTPS and UDP through the Floating IP see the client", func(t *testing.T) {
			serveHTTPS(t, tofu)
			udpSeesClient(t, tofu)
		}},
		{"an agent restart leaves the containers alone", func(t *testing.T) {
			before := stackContainer(t, server, "solo")
			server.must(t, "systemctl restart clowderd")
			time.Sleep(agentPass)
			server.must(t, "systemctl is-active clowderd")
			if after := stackContainer(t, server, "solo"); after != before {
				t.Errorf("the agent's restart recreated solo's container: %q, then %q", before, after)
			}
			if phase, err := phaseOf(firstHost); err != nil || phase != "active" {
				t.Errorf("%s is %s (%v), want active", firstHost, phase, err)
			}
		}},
		{"after a reboot the host leads again and its stacks come back", func(t *testing.T) { rebootAlone(t, server) }},
		{"the stacks and the ACME settings go", func(t *testing.T) {
			for _, name := range []string{"echo", "solo", "loose"} {
				removeStack(t, name)
			}
			changeConfig(t, func(config *clusterspec.Config) { config.ACME = clusterspec.ACME{} })
		}},
	})
}

// aloneWithoutAlert checks the cluster of one host: it votes and leads,
// no alert is open (no-ha comes only after clowderd recover), the
// credentials name its address alone, and it still issues join tokens.
func aloneWithoutAlert(t *testing.T, tofu outputs) {
	hosts := clusterHosts(t)
	if len(hosts) != 1 || hosts[0].Name != firstHost || !hosts[0].Voter || hosts[0].Phase != "active" {
		t.Errorf("the cluster's hosts: %+v, want %s alone, active and voting", hosts, firstHost)
	}
	if leader := leaderName(t); leader != firstHost {
		t.Errorf("the leader is %s, want %s", leader, firstHost)
	}
	if status := clusterStatus(t); !strings.Contains(status, "no open alert") {
		t.Errorf("a cluster of one host has an open alert:\n%s", status)
	}
	config, err := clientconfig.Read(credentialsFile())
	if err != nil {
		t.Fatal(err)
	}
	cliContext, err := config.Context("")
	if err != nil {
		t.Fatal(err)
	}
	if endpoints := cliContext.Endpoints; len(endpoints) != 1 || endpoints[0] != tofu.PublicAddress.Value {
		t.Errorf("the credentials name the endpoints %v, want %s alone", endpoints, tofu.PublicAddress.Value)
	}
	joinToken(t, 10*time.Minute, allRoles)
}

// runAlone runs solo, with a volume of two replicas, and loose, with a
// volume of replicas 0. solo's volume has its one copy and no tiebreaker,
// as there is no other host, and that raises no alert.
func runAlone(t *testing.T, server host) {
	createAndApply(t, "solo", volumeDocument("solo", "fast", failoverVolumeSize, 2)+simpleStack("solo", "solo"))
	createAndApply(t, "loose", volumeDocument("loose", "bulk", failoverVolumeSize, 0)+simpleStack("loose", "loose"))
	waitRunning(t, "solo", 1)
	waitRunning(t, "loose", 1)
	status := volumeStatus(t, "solo")
	if status.Status != "degraded" || len(status.Copies) != 1 || status.Copies[0].Host != firstHost || status.Copies[0].Tiebreaker {
		t.Errorf("solo's volume is %s with the copies %+v, want degraded with one copy on %s", status.Status,
			status.Copies, firstHost)
	}
	writeProof(t, server, "solo", "alone")
	if status := clusterStatus(t); !strings.Contains(status, "no open alert") {
		t.Errorf("a volume with one copy on a cluster of one host raised an alert:\n%s", status)
	}
}

// rebootAlone reboots the only host: the cluster is away while it boots,
// and after it the host leads again and runs its stacks with their data.
func rebootAlone(t *testing.T, server host) {
	boot := bootID(t, server)
	// in the background, so ssh returns before the connection goes
	server.must(t, "nohup sh -c 'sleep 2; systemctl reboot' >/dev/null 2>&1 &")
	eventually(t, rebootDeadline, "the host booted again", func() error {
		now, err := server.run("cat /proc/sys/kernel/random/boot_id")
		if err == nil && now == boot {
			err = fmt.Errorf("still the boot %s", boot)
		}
		return err
	})
	eventually(t, rebootDeadline, "solo running after the reboot", func() error {
		if stackContainer(t, server, "solo") == "" {
			return fmt.Errorf("no container of solo")
		}
		// the agent may not answer yet
		stacks, err := listStacks()
		if err == nil && stacks["solo"].Status != "running" {
			err = fmt.Errorf("solo is %s", stacks["solo"].Status)
		}
		return err
	})
	checkProof(t, server, "solo", "alone")
	if leader := leaderName(t); leader != firstHost {
		t.Errorf("the leader after the reboot is %s, want %s", leader, firstHost)
	}
	if !server.runs(t, "loose", "app") {
		t.Error("loose does not run after the reboot")
	}
}
