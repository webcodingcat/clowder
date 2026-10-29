//go:build live

// Package live tests clowder on a real host: the server of examples/hetzner
// in HLab, set up by the module's cloud-init and driven with the real CLI
// and SSH. bin/dev live makes the host and runs these tests in the lab's
// network (DEVELOPING.md, Live tests).
package live

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"go.yaml.in/yaml/v3"

	"github.com/webcodingcat/clowder/internal/clusterdir"
)

// The host's files, as setup.sh and the agent lay them out. They are copied,
// not imported: the agent keeps the layout to itself, and the test checks the
// host as a user would find it.
const (
	setupMarker       = "/var/lib/clowder-setup/done"
	renderedStackFile = "/var/lib/clowder/stacks/shop/compose.yaml"
	stackSecretsDir   = "/run/clowder-secrets/shop"
)

// shopStack is the test's stack: a container that counts its starts on a
// replicated volume and reads a secret as a file and one as a variable.
const shopStack = `name: shop
x-clowder:
  secrets_file: secrets.sops.yaml
services:
  app:
    image: alpine:3.22
    command: ["sh", "-c", "date >> /data/starts && exec sleep infinity"]
    volumes: [data:/data, scratch:/scratch]
    x-clowder:
      secrets: [password]
      secret_env: {API_TOKEN: api-token}
volumes:
  data:
    x-clowder: {class: fast, size: 1G, replicas: 2}
  scratch:
    x-clowder: {class: bulk, size: 1G, replicas: 0}
`

// The deadlines of the steps. The setup installs packages, builds DRBD's
// module and may reboot into a newer kernel: about 5 minutes without a
// reboot, the deadline leaves room for two and a slow mirror.
const (
	setupDeadline  = 40 * time.Minute
	stackDeadline  = 10 * time.Minute
	rebootDeadline = 10 * time.Minute
)

// TestMain gives the test its own age key: the CLI and the test read the
// SOPS files with it.
func TestMain(m *testing.M) {
	if workDir == "" || clowderBinary == "" {
		fmt.Fprintln(os.Stderr, "set LIVE_DIR and CLOWDER; bin/dev live does")
		os.Exit(2)
	}
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.Setenv("SOPS_AGE_KEY", identity.String()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	recipient = identity.Recipient().String()
	os.Exit(m.Run())
}

// recipient is the public half of the test's age key.
var recipient string

// TestOneHost runs a host's way from the first boot to a deleted stack. The
// steps build on each other, so the first failure ends the test.
func TestOneHost(t *testing.T) {
	tofu := readOutputs(t)
	server := host{address: tofu.PublicAddress.Value}
	t.Cleanup(func() {
		if t.Failed() {
			diagnose(t, server)
		}
	})
	steps := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"the setup makes the first host", func(t *testing.T) { waitForSetup(t, server) }},
		{"the user bootstraps and applies the cluster", func(t *testing.T) { bootstrap(t, tofu) }},
		{"a stack runs with volumes and secrets", func(t *testing.T) { runStack(t, server) }},
		{"the stack comes back after a reboot", func(t *testing.T) { reboot(t, server, tofu) }},
		{"a changed secret recreates the container", func(t *testing.T) { changeSecrets(t, server) }},
		{"compose fails on a missing secret file", func(t *testing.T) { missingSecretFile(t, server) }},
		{"the stack is deleted with its volumes", func(t *testing.T) { deleteStack(t, server) }},
		{"the agent's memory", func(t *testing.T) { logMemory(t, server) }},
	}
	for _, step := range steps {
		if !t.Run(step.name, step.run) {
			return
		}
	}
}

// waitForSetup waits until clowder-setup has made the host the first of a
// cluster and the agent runs.
func waitForSetup(t *testing.T, server host) {
	eventually(t, setupDeadline, "clowder-setup done", func() error {
		_, err := server.run("test -e " + setupMarker)
		return err
	})
	server.must(t, "systemctl is-active clowderd")
	if _, err := server.run("test -e /etc/clowder/bootstrap-key"); err == nil {
		t.Error("the bootstrap key file is still on the host")
	}
}

// bootstrap makes the test the first user, fills in cluster.yaml from the
// module's outputs and sets the hoster token in secrets.sops.yaml, keeping
// the join keys, then applies both.
func bootstrap(t *testing.T, tofu outputs) {
	keyFile := filepath.Join(workDir, "bootstrap-key")
	if err := os.WriteFile(keyFile, []byte(tofu.BootstrapKey.Value), 0o600); err != nil {
		t.Fatal(err)
	}
	mustClowder(t, "cluster", "bootstrap", "tester", "--age", recipient,
		"--seed", tofu.PublicAddress.Value, "--key-file", keyFile)

	// the module's part of cluster.yaml, read strictly into the real type, so a
	// renamed field fails here and not only in the cluster
	fromModule, err := yaml.Marshal(tofu.ClusterYAML.Value)
	if err != nil {
		t.Fatal(err)
	}
	var config clusterdir.Config
	decoder := yaml.NewDecoder(bytes.NewReader(fromModule))
	decoder.KnownFields(true)
	if err := decoder.Decode(&config); err != nil {
		t.Fatalf("the module's cluster_yaml: %v", err)
	}
	config.Name = "live"
	config.Hoster = "hetzner"
	config.HosterToken = clusterdir.SecretRef{Secret: "hcloud-token"}
	content, err := yaml.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clusterDir(), clusterdir.ConfigFile), content, 0o644); err != nil {
		t.Fatal(err)
	}

	secretsFile := filepath.Join(clusterDir(), clusterdir.SecretsFile)
	secrets := readSecrets(t, secretsFile)
	secrets.Values["hcloud-token"] = os.Getenv("HCLOUD_TOKEN")
	if err := clusterdir.WriteSOPS(secretsFile, secrets, recipient); err != nil {
		t.Fatal(err)
	}
	if joinKeys := readSecrets(t, secretsFile).JoinKeys; !slices.Equal(joinKeys, []string{tofu.BootstrapKey.Value}) {
		t.Fatalf("secrets.sops.yaml lost the bootstrap key from its join keys")
	}

	mustClowder(t, "cluster", "apply")
	status := mustClowder(t, "cluster", "status")
	if strings.Contains(status, "leader none") {
		t.Errorf("the cluster has no leader:\n%s", status)
	}
}

func readSecrets(t *testing.T, file string) clusterdir.Secrets {
	t.Helper()
	plain, err := clusterdir.ReadSOPS(file)
	if err != nil {
		t.Fatal(err)
	}
	var secrets clusterdir.Secrets
	if err := yaml.Unmarshal(plain, &secrets); err != nil {
		t.Fatal(err)
	}
	return secrets
}

// The stack's secrets in each generation; a $ must reach the container as
// it is.
var (
	firstSecrets  = map[string]string{"password": "first pa$$word", "api-token": "tok$en 'one'"}
	secondSecrets = map[string]string{"password": "second pa$$word", "api-token": "tok$en 'two'"}
)

// runStack creates the stack, applies it and checks the container: its
// secrets, its volume, both pools.
func runStack(t *testing.T, server host) {
	mustClowder(t, "stack", "create", "shop", "--dir", stackDir(), "--age", recipient)
	if err := os.WriteFile(filepath.Join(stackDir(), "clowder.yaml"), []byte(shopStack), 0o644); err != nil {
		t.Fatal(err)
	}
	writeStackSecrets(t, firstSecrets)
	mustClowder(t, "stack", "apply", "shop", "--file", filepath.Join(stackDir(), "clowder.yaml"))
	waitRunning(t, "shop", 1, stackDeadline)
	mustClowder(t, "stack", "ps", "shop")

	checkSecrets(t, server, firstSecrets)
	checkStarts(t, server, 1)
	// data (replicated) went to the pool of fast, scratch to the one of bulk
	for _, pool := range []string{"vg-fast", "vg-bulk"} {
		server.must(t, "lvs --noheadings -o lv_name "+pool+" | grep -q shop")
	}
	server.must(t, "drbdsetup status | grep -q shop")
}

func writeStackSecrets(t *testing.T, values map[string]string) {
	t.Helper()
	if err := clusterdir.WriteSOPS(filepath.Join(stackDir(), "secrets.sops.yaml"), values, recipient); err != nil {
		t.Fatal(err)
	}
}

// appContainer is the ID of the stack's container.
func appContainer(t *testing.T, server host) string {
	t.Helper()
	id := server.must(t, "docker ps -q --filter label=com.docker.compose.project=shop "+
		"--filter label=com.docker.compose.service=app")
	if id == "" {
		t.Fatal("the stack's container does not run")
	}
	return id
}

func checkSecrets(t *testing.T, server host, want map[string]string) {
	t.Helper()
	id := appContainer(t, server)
	if got := server.must(t, "docker exec "+id+" cat /run/secrets/password"); got != want["password"] {
		t.Errorf("/run/secrets/password = %q, want %q", got, want["password"])
	}
	if got := server.must(t, "docker exec "+id+" printenv API_TOKEN"); got != want["api-token"] {
		t.Errorf("API_TOKEN = %q, want %q", got, want["api-token"])
	}
}

// checkStarts checks how often the container started on its volume: the
// data stays across reboots and new generations.
func checkStarts(t *testing.T, server host, want int) {
	t.Helper()
	got := server.must(t, "docker exec "+appContainer(t, server)+" sh -c 'wc -l < /data/starts'")
	if got != fmt.Sprint(want) {
		t.Errorf("the container started %s times on its volume, want %d", got, want)
	}
}

// reboot restarts the host: after the boot, the agent brings the stack back,
// and the operator base (the Floating IP, the pools, the watchdog) is still
// there.
func reboot(t *testing.T, server host, tofu outputs) {
	before := server.must(t, "cat /proc/sys/kernel/random/boot_id")
	// in the background, so ssh returns before the connection goes
	server.must(t, "nohup sh -c 'sleep 2; systemctl reboot' >/dev/null 2>&1 &")
	eventually(t, rebootDeadline, "the host booted anew", func() error {
		after, err := server.run("cat /proc/sys/kernel/random/boot_id")
		if err == nil && after == before {
			return fmt.Errorf("still the old boot")
		}
		return err
	})
	waitRunning(t, "shop", 1, stackDeadline)
	eventually(t, stackDeadline, "the stack's container runs", func() error {
		_, err := server.run("docker ps -q --filter label=com.docker.compose.project=shop | grep -q .")
		return err
	})
	checkSecrets(t, server, firstSecrets)
	checkStarts(t, server, 2)
	classes, ok := tofu.ClusterYAML.Value["storage_classes"].(map[string]any)
	if !ok {
		t.Fatalf("the module's cluster_yaml has no storage_classes")
	}
	var pools []string
	for class := range classes {
		pools = append(pools, "--pool vg-"+class+"/thin")
	}
	// the time sync comes a while after the boot; the rest must hold at once
	check := "clowderd check " + strings.Join(pools, " ") + " --floating-ip " + tofu.FloatingIP.Value
	eventually(t, rebootDeadline, "the time is synchronized", func() error {
		_, err := server.run("timedatectl show --property=NTPSynchronized --value | grep -qx yes")
		return err
	})
	server.must(t, check)
}

// changeSecrets applies the stack with new secrets: a new generation, whose
// label makes compose recreate the container with the new values.
func changeSecrets(t *testing.T, server host) {
	before := appContainer(t, server)
	writeStackSecrets(t, secondSecrets)
	mustClowder(t, "stack", "apply", "shop", "--file", filepath.Join(stackDir(), "clowder.yaml"))
	waitRunning(t, "shop", 2, stackDeadline)
	if after := appContainer(t, server); after == before {
		t.Error("the container was not recreated")
	}
	checkSecrets(t, server, secondSecrets)
	checkStarts(t, server, 3)
}

// missingSecretFile runs compose up with the agent's files and a secret file
// gone: compose must fail rather than start the container without the
// secret, which the agent relies on (DEVELOPING.md, The agent). The agent is
// stopped meanwhile, so neither it nor its supervisor acts on what compose
// does; the containers keep running (KillMode=process). Started again, the
// agent writes the secrets anew and brings the stack up as after a boot.
func missingSecretFile(t *testing.T, server host) {
	server.must(t, "systemctl stop clowderd")
	server.must(t, "rm "+stackSecretsDir+"/files/password")
	output, err := server.run("docker compose --project-name shop --file " + renderedStackFile +
		" --file " + stackSecretsDir + "/env.yaml up --detach --force-recreate")
	if err == nil {
		t.Errorf("compose up started the stack without its secret file:\n%s", output)
	} else {
		t.Logf("compose up without the secret file failed as it should:\n%s", output)
	}
	server.must(t, "systemctl start clowderd")
	eventually(t, stackDeadline, "the agent brought the stack back", func() error {
		_, err := server.run("docker ps -q --filter label=com.docker.compose.project=shop | grep -q .")
		return err
	})
	checkSecrets(t, server, secondSecrets)
}

// deleteStack deletes the stack with its volumes: the containers, the LVs
// and the DRBD resource go.
func deleteStack(t *testing.T, server host) {
	mustClowder(t, "stack", "delete", "shop", "--delete-volumes")
	eventually(t, stackDeadline, "the stack is gone", func() error {
		_, found, err := findStack("shop")
		if err == nil && found {
			return fmt.Errorf("still listed")
		}
		return err
	})
	eventually(t, stackDeadline, "the host removed the stack", func() error {
		output, err := server.run("docker ps -aq --filter label=com.docker.compose.project=shop; " +
			"lvs --noheadings -o lv_name | grep shop; drbdsetup status | grep shop; true")
		if err == nil && output != "" {
			return fmt.Errorf("left on the host:\n%s", output)
		}
		return err
	})
}

// logMemory logs what the agent uses; it is a measurement, not a check.
func logMemory(t *testing.T, server host) {
	t.Logf("clowderd: resident %s KiB, its cgroup %s bytes",
		server.must(t, "ps -o rss= -C clowderd"),
		server.must(t, "systemctl show clowderd --property=MemoryCurrent --value"))
}
