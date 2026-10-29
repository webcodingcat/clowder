//go:build live

package live

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// What bin/dev live hands over: the work directory (OpenTofu's outputs, the
// SSH key) and the CLI to test.
var (
	workDir       = os.Getenv("LIVE_DIR")
	clowderBinary = os.Getenv("CLOWDER")
)

// commandTimeout ends a single ssh or clowder call that hangs; the steps have
// deadlines of their own.
const commandTimeout = 5 * time.Minute

// outputs are the outputs of examples/hetzner (tofu output -json).
type outputs struct {
	PublicAddress struct{ Value string }         `json:"public_address"`
	FloatingIP    struct{ Value string }         `json:"floating_ip"`
	BootstrapKey  struct{ Value string }         `json:"bootstrap_key"`
	ClusterYAML   struct{ Value map[string]any } `json:"cluster_yaml"`
}

func readOutputs(t *testing.T) outputs {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(workDir, "outputs.json"))
	if err != nil {
		t.Fatal(err)
	}
	var result outputs
	if err := json.Unmarshal(content, &result); err != nil {
		t.Fatalf("read OpenTofu's outputs: %v", err)
	}
	return result
}

// host is the test's server, reached over SSH as root with the key OpenTofu
// gave it.
type host struct {
	address string
}

// run runs a shell command on the host and returns its output.
func (h host) run(command string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	ssh := exec.CommandContext(ctx, "ssh", "-i", filepath.Join(workDir, "id_ed25519"),
		"-o", "BatchMode=yes", "-o", "ConnectTimeout=10",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "UserKnownHostsFile="+filepath.Join(workDir, "known_hosts"),
		"root@"+h.address, command)
	output, err := ssh.CombinedOutput()
	trimmed := strings.TrimSpace(string(output))
	if err != nil {
		return trimmed, fmt.Errorf("on the host: %s: %w\n%s", command, err, trimmed)
	}
	return trimmed, nil
}

// must runs a shell command on the host and fails the test if it fails.
func (h host) must(t *testing.T, command string) string {
	t.Helper()
	output, err := h.run(command)
	if err != nil {
		t.Fatal(err)
	}
	return output
}

// clowder runs the CLI against the test's cluster directory.
func clowder(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	cli := exec.CommandContext(ctx, clowderBinary, args...)
	cli.Dir = workDir
	cli.Env = append(os.Environ(), "CLOWDER_CLUSTER="+clusterDir())
	output, err := cli.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("clowder %s: %w\n%s", strings.Join(args, " "), err, output)
	}
	return string(output), nil
}

// mustClowder runs the CLI and fails the test if it fails.
func mustClowder(t *testing.T, args ...string) string {
	t.Helper()
	output, err := clowder(args...)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("clowder %s:\n%s", strings.Join(args, " "), output)
	return output
}

func clusterDir() string { return filepath.Join(workDir, "cluster") }

func stackDir() string { return filepath.Join(workDir, "shop") }

// eventually calls check until it succeeds, and fails the test with the last
// error once the deadline has passed.
func eventually(t *testing.T, deadline time.Duration, what string, check func() error) {
	t.Helper()
	end := time.Now().Add(deadline)
	for {
		err := check()
		if err == nil {
			return
		}
		if time.Now().After(end) {
			t.Fatalf("%s: not within %s: %v", what, deadline, err)
		}
		time.Sleep(5 * time.Second)
	}
}

// stackRow is a stack's line of clowder stack ps.
type stackRow struct {
	status     string
	generation int
}

// findStack reads clowder stack ps; found is false when the cluster does not
// list the stack.
func findStack(name string) (row stackRow, found bool, err error) {
	output, err := clowder("stack", "ps")
	if err != nil {
		return stackRow{}, false, err
	}
	for _, line := range strings.Split(output, "\n") {
		// NAME STATUS HOST PRIORITY GENERATION ...; a running stack has a host
		fields := strings.Fields(line)
		if len(fields) < 5 || fields[0] != name {
			continue
		}
		row.status = fields[1]
		if fields[1] == "running" {
			if row.generation, err = strconv.Atoi(fields[4]); err != nil {
				return stackRow{}, false, fmt.Errorf("read the generation in %q: %w", line, err)
			}
		}
		return row, true, nil
	}
	return stackRow{}, false, nil
}

// waitRunning waits until the stack runs in the given generation.
func waitRunning(t *testing.T, name string, generation int, deadline time.Duration) {
	t.Helper()
	eventually(t, deadline, fmt.Sprintf("stack %s running in generation %d", name, generation), func() error {
		row, found, err := findStack(name)
		switch {
		case err != nil:
			return err
		case !found:
			return fmt.Errorf("not listed")
		case row.status != "running" || row.generation != generation:
			return fmt.Errorf("%s, generation %d", row.status, row.generation)
		}
		return nil
	})
}

// diagnose prints what tells why the host is not where the test expected it.
func diagnose(t *testing.T, server host) {
	for _, command := range []string{
		"cloud-init status --long",
		// across boots: the setup may have rebooted the host
		"journalctl --no-pager -u clowder-setup -n 150",
		"journalctl --no-pager --boot -u clowderd -n 150",
		"docker ps -a",
	} {
		output, _ := server.run(command)
		t.Logf("%s:\n%s", command, output)
	}
}
