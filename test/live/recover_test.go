//go:build live

package live

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestRecover takes the cluster through the loss of its majority for good
// and clowderd recover --power-off on the first host:
//   - the stack keeper (replicas 2, a copy on the first host) runs there with
//     its data and writes again; the stack lost (replicas 1, its copy on a
//     joined host only) waits for a restore;
//   - the other hosts are off, their servers labeled clowder-retired, removed
//     from the cluster; the first host leads alone and holds the Floating IP,
//     and the alert no-ha is open;
//   - switched on again, the retired servers start neither Raft nor a stack;
//   - new servers under the same names join, and the cluster is highly
//     available again.
func TestRecover(t *testing.T) {
	server, tofu := baseline(t)
	hosts := scenarioHosts(server, tofu)
	joined := undrainJoined(t)
	slices.Sort(joined)
	cleanUpFaults(t)
	runSteps(t, []step{
		{"a stack with a copy on the first host, one with its data on a joined host only", func(t *testing.T) {
			createAndApply(t, "keeper", volumeDocument("keeper", "fast", failoverVolumeSize, 2)+simpleStack("keeper", "keeper"))
			waitRunning(t, "keeper", 1)
			moveTo(t, "keeper", firstHost)
			volumeSteady(t, hosts, "keeper")
			writeProof(t, server, "keeper", "before")
			createAndApply(t, "lost", volumeDocument("lost", "fast", failoverVolumeSize, 1)+simpleStack("lost", "lost"))
			waitRunning(t, "lost", 1)
			moveTo(t, "lost", joined[0])
			waitSynced(t, "lost")
		}},
		{"the other hosts go, and recover on the first host goes on alone", func(t *testing.T) {
			for _, name := range joined {
				labControl(t, "/vm/"+name+"/pause", nil)
			}
			output := server.must(t, "clowderd recover --power-off --yes")
			t.Logf("clowderd recover:\n%s", output)
			for _, name := range joined {
				if !strings.Contains(output, "Host "+name+" (server ") {
					t.Errorf("recover does not say it retired %s", name)
				}
			}
		}},
		{"the others are off, retired and removed; the first host leads alone", func(t *testing.T) {
			eventually(t, time.Minute, "a leader", func() error {
				if leader := leaderName(t); leader != firstHost {
					return fmt.Errorf("the leader is %s", leader)
				}
				return nil
			})
			for _, name := range joined {
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
			eventually(t, placementDeadline, "the Floating IP on "+firstHost, func() error {
				if now := ingressHost(t, tofu.FloatingIP.Value); now != firstHost {
					return fmt.Errorf("it is on %q", now)
				}
				return nil
			})
			if status := mustClowder(t, "cluster", "status"); !strings.Contains(status, "not highly available") {
				t.Errorf("no alert no-ha:\n%s", status)
			}
		}},
		{"keeper runs with its data and writes; lost waits for a restore", func(t *testing.T) {
			waitRunning(t, "keeper", 1)
			runsOnce(t, map[string]host{firstHost: server}, "keeper", "before")
			// a write that hung without quorum would end here
			server.must(t, "timeout 60 docker exec "+stackContainer(t, server, "keeper")+
				" sh -c 'echo after > /data/proof && sync'")
			checkProof(t, server, "keeper", "after")
			if status := volumeStatus(t, "lost"); status.Status != "restore-pending" || len(status.Copies) != 0 {
				t.Errorf("volume lost: %s, copies %+v", status.Status, status.Copies)
			}
			eventually(t, time.Minute, "lost waiting for a restore", func() error {
				if listed := stackInfo(t, "lost"); !strings.Contains(listed.WaitReason, "waits for a restore") {
					return fmt.Errorf("%s on %q (%s)", listed.Status, listed.Host, listed.WaitReason)
				}
				return nil
			})
		}},
		{"the retired servers, switched on, start neither Raft nor a stack", func(t *testing.T) {
			for _, name := range joined {
				powerOn(t, name)
			}
			for _, name := range joined {
				retiredStartsNothing(t, hosts[name], name)
			}
			if phase, err := phaseOf(firstHost); err != nil || phase != "active" || leaderName(t) != firstHost {
				t.Errorf("%s is %s (%v) after the retired servers came up", firstHost, phase, err)
			}
		}},
		{"new hosts join, and the cluster is highly available again", func(t *testing.T) {
			removeStack(t, "keeper")
			removeStack(t, "lost")
			// empty, not nil: OpenTofu iterates over the lists
			applyHosts(t, []string{}, map[string]string{})
			tokens := map[string]string{}
			for _, name := range joined {
				tokens[name] = joinToken(t, time.Hour)
			}
			applyHosts(t, joined, tokens)
			waitForJoined(t, joined...)
			eventually(t, time.Minute, "three voters", func() error { return checkThreeVoters(clusterHosts(t)) })
			eventually(t, time.Minute, "the alert no-ha resolved", func() error {
				if status := mustClowder(t, "cluster", "status"); strings.Contains(status, "not highly available") {
					return fmt.Errorf("it is open:\n%s", status)
				}
				return nil
			})
			drainJoined(t)
		}},
	})
}

// retiredStartsNothing checks a retired server once it is up: its agent
// ended with status 4 and says why, nothing listens on Raft's port, and no
// container or DRBD resource runs.
func retiredStartsNothing(t *testing.T, retired host, name string) {
	t.Helper()
	eventually(t, setupDeadline, name+" up again", func() error {
		_, err := retired.run("systemctl is-system-running --wait >/dev/null; true")
		return err
	})
	eventually(t, time.Minute, name+"'s agent stopped for good", func() error {
		status := retired.must(t, "systemctl show -p ExecMainStatus --value clowderd")
		if status != "4" {
			return fmt.Errorf("clowderd exited with %q", status)
		}
		return nil
	})
	if journal := retired.must(t, "journalctl -b -u clowderd --no-pager"); !strings.Contains(journal, "clowder-retired") {
		t.Errorf("%s's journal does not say it is retired:\n%s", name, journal)
	}
	if listening := retired.must(t, "ss -Hltn 'sport = :7400'"); listening != "" {
		t.Errorf("%s listens on Raft's port: %s", name, listening)
	}
	if containers := retired.must(t, "docker ps -q"); containers != "" {
		t.Errorf("%s runs containers: %s", name, containers)
	}
	// without a resource drbdsetup prints only a comment
	if resources := retired.must(t, "drbdsetup status | grep -v '^#' || true"); resources != "" {
		t.Errorf("%s has DRBD resources up: %s", name, resources)
	}
}

// serverLabels are the labels of the host's server at the hoster.
func serverLabels(t *testing.T, name string) map[string]string {
	t.Helper()
	id, _ := serverOf(t, name)
	var answer struct {
		Server struct {
			Labels map[string]string `json:"labels"`
		} `json:"server"`
	}
	cloud(t, http.MethodGet, fmt.Sprintf("/servers/%d", id), nil, &answer)
	return answer.Server.Labels
}

// powerOn switches the host's server on.
func powerOn(t *testing.T, name string) {
	t.Helper()
	id, _ := serverOf(t, name)
	cloud(t, http.MethodPost, fmt.Sprintf("/servers/%d/actions/poweron", id), nil, nil)
}
