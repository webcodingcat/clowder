//go:build live

package live

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestRolesRecover takes the lab of roles through the loss of two of its
// three control-plane hosts for good and clowderd recover --power-off on
// the first host:
//   - the stack keeper (replicas 2, its copies on both workers) runs on a
//     worker in the same container, writes, and keeps both copies synced,
//     with its tiebreaker on the first host;
//   - the two other control-plane hosts are off, their servers labeled
//     clowder-retired, removed from the cluster; the first host leads alone,
//     and the alert no-ha is open;
//   - both workers stay active, without a vote, and follow the first host
//     as fresh Raft members (their old Raft log set aside); the Floating IP
//     stays on an ingress worker;
//   - new control-plane hosts join under the old names, and the cluster is
//     highly available again.
func TestRolesRecover(t *testing.T) {
	server, tofu := baseline(t, splitRoles)
	hosts := scenarioHosts(server, tofu)
	layout := labLayout(t)
	retired := []string{firstJoining, secondJoining}
	workers := []string{thirdJoining, replacement}
	cleanUpFaults(t)
	// the worker that runs keeper, and keeper's container there
	var worker, container string
	runSteps(t, []step{
		{"keeper runs on a worker, with its copies on both workers", func(t *testing.T) {
			createAndApply(t, "keeper", volumeDocument("keeper", "fast", failoverVolumeSize, 2)+simpleStack("keeper", "keeper"))
			waitRunning(t, "keeper", 1)
			volumeSteady(t, hosts, "keeper")
			checkCopiesOnWorkers(t, layout, "keeper")
			worker = stackInfo(t, "keeper").Host
			writeProof(t, hosts[worker], "keeper", "before")
			container = stackContainer(t, hosts[worker], "keeper")
		}},
		{"two control-plane hosts go, and recover on the first host keeps the workers", func(t *testing.T) {
			for _, name := range retired {
				labControl(t, "/vm/"+name+"/pause", nil)
			}
			output := server.must(t, "clowderd recover --power-off --yes")
			t.Logf("clowderd recover:\n%s", output)
			for _, name := range retired {
				if !strings.Contains(output, "Host "+name+" (server ") {
					t.Errorf("recover does not say it retired %s", name)
				}
			}
			for _, name := range workers {
				if !strings.Contains(output, "Host "+name+" stays and follows this host.") {
					t.Errorf("recover does not say that %s stays", name)
				}
			}
		}},
		{"the first host leads alone; the others are off, retired and removed", func(t *testing.T) {
			eventually(t, time.Minute, "a leader", func() error {
				if leader := leaderName(t); leader != firstHost {
					return fmt.Errorf("the leader is %s", leader)
				}
				return nil
			})
			for _, name := range retired {
				if status := serverStatus(t, name); status != "off" {
					t.Errorf("%s's server is %s", name, status)
				}
				if labels := serverLabels(t, name); labels["clowder-retired"] == "" {
					t.Errorf("%s's server has the labels %v", name, labels)
				}
				if phase, err := phaseOf(name); err != nil || phase != "removed" {
					t.Errorf("%s is %s (%v)", name, phase, err)
				}
			}
			if status := mustClowder(t, "cluster", "status"); !strings.Contains(status, "not highly available") {
				t.Errorf("no alert no-ha:\n%s", status)
			}
		}},
		{"the workers stay active, without a vote, as fresh Raft members", func(t *testing.T) {
			eventually(t, 5*time.Minute, "the workers active", func() error {
				listed := clusterHosts(t)
				for _, name := range workers {
					found, ok := findHost(listed, name)
					if !ok || found.Phase != "active" || found.Voter {
						return fmt.Errorf("%s is %+v", name, found)
					}
				}
				return nil
			})
			for _, name := range workers {
				if aside := hosts[name].must(t, "ls -d /var/lib/clowder/raft.set-aside-* 2>/dev/null || true"); aside == "" {
					t.Errorf("%s did not set its Raft log aside", name)
				}
			}
		}},
		{"keeper runs on in its container, with both copies and the tiebreaker on the first host", func(t *testing.T) {
			if now := stackInfo(t, "keeper").Host; now != worker {
				t.Fatalf("keeper moved from %s to %s", worker, now)
			}
			if now := stackContainer(t, hosts[worker], "keeper"); now != container {
				t.Errorf("keeper's container changed: %s, then %s", container, now)
			}
			// a write that hung without quorum would end here
			hosts[worker].must(t, "timeout 60 docker exec "+container+" sh -c 'echo after > /data/proof && sync'")
			checkProof(t, hosts[worker], "keeper", "after")
			volumeSteady(t, hosts, "keeper")
			checkCopiesOnWorkers(t, layout, "keeper")
			for _, volumeCopy := range volumeStatus(t, "keeper").Copies {
				if volumeCopy.Tiebreaker && volumeCopy.Host != firstHost {
					t.Errorf("keeper's tiebreaker is on %s, want %s", volumeCopy.Host, firstHost)
				}
			}
			eventually(t, placementDeadline, "the Floating IP on an ingress worker", func() error {
				if now := ingressHost(t, tofu.FloatingIP.Value); !slices.Contains(workers, now) {
					return fmt.Errorf("it is on %q", now)
				}
				return nil
			})
		}},
		{"new control-plane hosts join under the old names, and the cluster is highly available again", func(t *testing.T) {
			// the module takes the retired servers out, then makes new ones
			applyHosts(t, workers, map[string]string{})
			tokens := map[string]string{}
			for _, name := range retired {
				tokens[name] = joinToken(t, time.Hour, layout.rolesOf(name))
			}
			applyHosts(t, append(slices.Clone(retired), workers...), tokens)
			waitForJoined(t, retired...)
			eventually(t, time.Minute, "three voters", func() error { return checkVoters(clusterHosts(t), 3) })
			if now := stackContainer(t, hosts[worker], "keeper"); now != container {
				t.Errorf("keeper's container changed while the hosts joined: %s, then %s", container, now)
			}
			checkProof(t, hosts[worker], "keeper", "after")
			removeStack(t, "keeper")
			eventually(t, time.Minute, "the alert no-ha resolved", func() error {
				if status := mustClowder(t, "cluster", "status"); strings.Contains(status, "not highly available") {
					return fmt.Errorf("it is open:\n%s", status)
				}
				return nil
			})
		}},
	})
}
