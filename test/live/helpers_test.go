//go:build live

package live

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/webcodingcat/clowder/internal/api"
)

// What bin/dev live hands over: the work directory (OpenTofu's outputs, the
// SSH key, the registry's files) and the CLI to test.
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
	return h.runWithInput(command, nil)
}

// runWithInput runs a shell command on the host with input on its stdin.
func (h host) runWithInput(command string, input io.Reader) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	ssh := exec.CommandContext(ctx, "ssh", "-i", filepath.Join(workDir, "id_ed25519"),
		"-o", "BatchMode=yes", "-o", "ConnectTimeout=10",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "UserKnownHostsFile="+filepath.Join(workDir, "known_hosts"),
		"root@"+h.address, command)
	ssh.Stdin = input
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

// upload writes a local file to the host, making its directory.
func (h host) upload(t *testing.T, local, remote string) {
	t.Helper()
	file, err := os.Open(local)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }() // read only
	if _, err := h.runWithInput(fmt.Sprintf("mkdir -p %s && cat > %s", path.Dir(remote), remote), file); err != nil {
		t.Fatal(err)
	}
}

// container is what the test reads of docker inspect.
type container struct {
	ID    string
	State struct {
		Pid        int
		StartedAt  time.Time
		FinishedAt time.Time
		Health     *struct{ Status string }
	}
	HostConfig struct {
		RestartPolicy struct{ Name string }
	}
}

// inspect reads one container on the host.
func (h host) inspect(t *testing.T, id string) container {
	t.Helper()
	var containers []container
	if err := json.Unmarshal([]byte(h.must(t, "docker inspect "+id)), &containers); err != nil || len(containers) != 1 {
		t.Fatalf("docker inspect %s: %d containers, %v", id, len(containers), err)
	}
	return containers[0]
}

// service is the ID of the container of a service of shop, the test's
// stack; empty if none runs.
func (h host) service(t *testing.T, service string) string {
	t.Helper()
	return h.must(t, "docker ps -q --no-trunc --filter label=com.docker.compose.project=shop"+
		" --filter label=com.docker.compose.service="+service)
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

// stackDir is the directory of a stack: outside the cluster directory, as
// DEVELOPING.md asks.
func stackDir(name string) string { return filepath.Join(workDir, "stacks", name) }

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
		time.Sleep(2 * time.Second)
	}
}

// listStacks reads clowder stack ps --output json.
func listStacks() (map[string]api.StackInfo, error) {
	output, err := clowder("stack", "ps", "--output", "json")
	if err != nil {
		return nil, err
	}
	var stacks []api.StackInfo
	if err := json.Unmarshal([]byte(output), &stacks); err != nil {
		return nil, fmt.Errorf("read stack ps: %w", err)
	}
	listed := map[string]api.StackInfo{}
	for _, stack := range stacks {
		listed[stack.Name] = stack
	}
	return listed, nil
}

// generation is a stack's generation as the cluster lists it.
func generation(t *testing.T, name string) int64 {
	t.Helper()
	stacks, err := listStacks()
	if err != nil {
		t.Fatal(err)
	}
	return stacks[name].Generation
}

// waitRunning waits until the stack runs in the given generation.
func waitRunning(t *testing.T, name string, generation int64, deadline time.Duration) {
	t.Helper()
	eventually(t, deadline, fmt.Sprintf("stack %s running in generation %d", name, generation), func() error {
		stacks, err := listStacks()
		if err != nil {
			return err
		}
		stack, found := stacks[name]
		switch {
		case !found:
			return fmt.Errorf("not listed")
		case stack.Status != "running" || stack.Generation != generation:
			return fmt.Errorf("%s, generation %d", stack.Status, stack.Generation)
		}
		return nil
	})
}

// waitGone waits until the cluster no longer lists the stacks.
func waitGone(t *testing.T, deadline time.Duration, names ...string) {
	t.Helper()
	eventually(t, deadline, "stacks gone", func() error {
		stacks, err := listStacks()
		if err != nil {
			return err
		}
		for _, name := range names {
			if _, found := stacks[name]; found {
				return fmt.Errorf("%s still listed", name)
			}
		}
		return nil
	})
}

// unixTime reads a time in seconds since 1970 with a fraction ("1761744507.123456").
func unixTime(t *testing.T, text string) time.Time {
	t.Helper()
	seconds, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
	if err != nil {
		t.Fatalf("read the time %q: %v", text, err)
	}
	return time.UnixMicro(int64(seconds * 1e6))
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
