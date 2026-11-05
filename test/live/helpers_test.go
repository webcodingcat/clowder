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
	"github.com/webcodingcat/clowder/internal/clientconfig"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/resource"
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
	PublicAddress  struct{ Value string }                `json:"public_address"`
	PrivateAddress struct{ Value string }                `json:"private_address"`
	FloatingIP     struct{ Value string }                `json:"floating_ip"`
	ClusterYAML    struct{ Value map[string]any }        `json:"cluster_yaml"`
	Hosts          struct{ Value map[string]moduleHost } `json:"hosts"`
}

// moduleHost is a host as the module's output hosts has it.
type moduleHost struct {
	PublicAddress  string `json:"public_address"`
	PrivateAddress string `json:"private_address"`
	ServerID       string `json:"server_id"`
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

// setupReached tells whether clowder-setup has left its marker on the host:
// done for a host, prepared for an image (TestImage). While it has not,
// the error says what the setup does now, its journal's last line, for the
// log of a long wait (eventually).
func (h host) setupReached(marker string) func() error {
	return func() error {
		if _, err := h.run("test -e " + marker); err == nil {
			return nil
		}
		last, err := h.run("journalctl -u clowder-setup -n 1 --no-pager -o cat")
		if err != nil {
			return fmt.Errorf("the host does not answer yet: %w", err)
		}
		return fmt.Errorf("the setup is at: %s", last)
	}
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
	return h.must(t, "docker ps -q --no-trunc --filter label=com.docker.compose.project="+project("shop")+
		" --filter label=com.docker.compose.service="+service)
}

// clowder runs the CLI against the test's cluster directory.
func clowder(args ...string) (string, error) {
	return clowderWithInput(nil, args...)
}

// clowderWithInput runs the CLI with input on its stdin (apply -f -).
func clowderWithInput(input io.Reader, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	cli := exec.CommandContext(ctx, clowderBinary, args...)
	cli.Dir = workDir
	cli.Stdin = input
	cli.Env = cliEnvironment()
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

// cliEnvironment is the CLI's environment: the test's cluster directory,
// and a credentials file of the test's own, so a run neither finds nor
// overwrites those of the developer.
func cliEnvironment() []string {
	return append(os.Environ(), "CLOWDER_CLUSTER="+clusterDir(), clientconfig.EnvVar+"="+credentialsFile())
}

// credentialsFile is the test's credentials file, as ~/.clowder/config is
// the user's.
func credentialsFile() string { return filepath.Join(workDir, "clowder-config") }

// stackDir is the directory of a stack's resources: outside the cluster
// directory, which apply refuses.
func stackDir(name string) string { return filepath.Join(workDir, "stacks", name) }

// stackFile is the file of a stack's resources: the stack and its volumes.
func stackFile(name string) string { return filepath.Join(stackDir(name), name+".yaml") }

// document is a resource of the namespace default as a YAML document; spec
// is the spec's YAML, which goes indented below spec:.
func document(kind, name, spec string) string {
	return documentIn("", kind, name, spec)
}

// documentIn is a resource of a namespace as a YAML document; "" leaves
// the namespace out (default, or a namespace itself); spec "" is none.
func documentIn(namespace, kind, name, spec string) string {
	metadata := "{name: " + name + "}"
	if namespace != "" {
		metadata = "{namespace: " + namespace + ", name: " + name + "}"
	}
	written := "---\napiVersion: " + resource.APIVersion + "\nkind: " + kind + "\nmetadata: " + metadata + "\n"
	if spec == "" {
		return written
	}
	var indented strings.Builder
	for _, line := range strings.SplitAfter(strings.TrimRight(spec, "\n"), "\n") {
		indented.WriteString("  " + line)
	}
	return written + "spec:\n" + indented.String() + "\n"
}

// volumeDocument is a Volume resource of the namespace default.
func volumeDocument(name, class, size string, replicas int) string {
	return document(resource.KindVolume, name, fmt.Sprintf("class: %s\nsize: %s\nreplicas: %d", class, size, replicas))
}

// encryptSecret is a Secret of the namespace default with its value
// encrypted for it, as clowder secret encrypt writes it.
func encryptSecret(t *testing.T, name, value string) string {
	t.Helper()
	secret, err := clowderWithInput(strings.NewReader(value), "secret", "encrypt", name)
	if err != nil {
		t.Fatal(err)
	}
	return secret
}

// createAndApply writes a stack's resources (the stack and its volumes) to
// its file and applies them.
func createAndApply(t *testing.T, name, content string) {
	t.Helper()
	if err := os.MkdirAll(stackDir(name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stackFile(name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	mustClowder(t, "apply", "-f", stackFile(name))
}

// removeStack deletes the resources of a stack's file, the stack before its
// volumes and their data, and waits until the cluster no longer lists it.
func removeStack(t *testing.T, name string) {
	t.Helper()
	mustClowder(t, "delete", "--with-data", "-f", stackFile(name))
	waitGone(t, name)
}

// eventually calls check until it succeeds, and fails the test with the last
// error once the deadline has passed. While it waits, it says so every
// progressInterval with the last error, so a long wait (a host's setup)
// shows in the log as a wait, not as a run that hangs.
func eventually(t *testing.T, deadline time.Duration, what string, check func() error) {
	t.Helper()
	start := time.Now()
	end, nextReport := start.Add(deadline), start.Add(progressInterval)
	for {
		err := check()
		if err == nil {
			if waited := time.Since(start); waited >= progressInterval {
				t.Logf("%s after %s", what, waited.Round(time.Second))
			}
			return
		}
		if time.Now().After(end) {
			t.Fatalf("%s: not within %s: %v", what, deadline, err)
		}
		if time.Now().After(nextReport) {
			t.Logf("waiting for %s for %s of %s: %v", what, time.Since(start).Round(time.Second), deadline, err)
			nextReport = time.Now().Add(progressInterval)
		}
		time.Sleep(2 * time.Second)
	}
}

// progressInterval is how often eventually reports a wait.
const progressInterval = time.Minute

// listedStack is a stack as clowder get stacks lists it.
type listedStack struct {
	generation int64
	api.StackStatus
}

// listResources reads clowder get <kind> -o json for the namespace default.
func listResources(kind string) ([]resource.Object, error) {
	output, err := clowder("get", kind, "-o", "json")
	if err != nil {
		return nil, err
	}
	var objects []resource.Object
	if err := json.Unmarshal([]byte(output), &objects); err != nil {
		return nil, fmt.Errorf("read get %s: %w", kind, err)
	}
	return objects, nil
}

// listStacks reads the stacks of the namespace default by name.
func listStacks() (map[string]listedStack, error) {
	objects, err := listResources("stacks")
	if err != nil {
		return nil, err
	}
	listed := map[string]listedStack{}
	for _, object := range objects {
		stack := listedStack{generation: object.Metadata.Generation}
		if err := json.Unmarshal(object.Status, &stack.StackStatus); err != nil {
			return nil, fmt.Errorf("read the status of stack %s: %w", object.Metadata.Name, err)
		}
		listed[object.Metadata.Name] = stack
	}
	return listed, nil
}

// stackInfo is a stack as the cluster lists it; zero if it lists none.
func stackInfo(t *testing.T, name string) listedStack {
	t.Helper()
	stacks, err := listStacks()
	if err != nil {
		t.Fatal(err)
	}
	return stacks[name]
}

// secretDigests are the digests of the secrets of the namespace default by
// name.
func secretDigests(t *testing.T) map[string]string {
	t.Helper()
	objects, err := listResources("secrets")
	if err != nil {
		t.Fatal(err)
	}
	digests := map[string]string{}
	for _, object := range objects {
		var status api.SecretStatus
		if err := json.Unmarshal(object.Status, &status); err != nil {
			t.Fatalf("read the status of secret %s: %v", object.Metadata.Name, err)
		}
		digests[object.Metadata.Name] = status.Digest
	}
	return digests
}

// volumeLV is the LV of a volume of the namespace default: its name on the
// host, with the data generation the cluster gave it.
func volumeLV(t *testing.T, name string) string {
	t.Helper()
	return names.VolumeName(names.ID(names.Default, name), volumeStatus(t, name).DataGeneration)
}

// waitRunning waits until the stack runs in the given generation.
func waitRunning(t *testing.T, name string, generation int64) {
	t.Helper()
	eventually(t, stackDeadline, fmt.Sprintf("stack %s running in generation %d", name, generation), func() error {
		stacks, err := listStacks()
		if err != nil {
			return err
		}
		stack, found := stacks[name]
		switch {
		case !found:
			return fmt.Errorf("not listed")
		case stack.Status != "running" || stack.generation != generation:
			return fmt.Errorf("%s, generation %d", stack.Status, stack.generation)
		}
		return nil
	})
}

// waitSecrets waits until the stack's containers came up with the secrets
// of the given digests, by name. A changed secret changes no stack's
// generation: the host reports the digests its containers have.
func waitSecrets(t *testing.T, name string, digests map[string]string) {
	t.Helper()
	eventually(t, stackDeadline, fmt.Sprintf("stack %s up with its secrets", name), func() error {
		applied := stackInfo(t, name).AppliedSecrets
		for secret, digest := range digests {
			if applied[secret] != digest {
				return fmt.Errorf("secret %s has not reached the containers", secret)
			}
		}
		return nil
	})
}

// waitGone waits until the cluster no longer lists the stacks.
func waitGone(t *testing.T, names ...string) {
	t.Helper()
	eventually(t, stackDeadline, "stacks gone", func() error {
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
