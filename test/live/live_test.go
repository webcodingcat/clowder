//go:build live

// Package live tests clowder on a real host: the server of examples/hetzner
// in HLab, set up by the module's cloud-init and driven with the real CLI
// and SSH. bin/dev live makes the host and runs these tests in the lab's
// network (docs/live-tests.md).
package live

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/webcodingcat/clowder/internal/clusterdir"
	"github.com/webcodingcat/clowder/internal/clusterspec"
	"github.com/webcodingcat/clowder/internal/resource"
	"github.com/webcodingcat/clowder/internal/yamlpath"
)

// The host's files, as setup.sh and the agent lay them out. They are copied,
// not imported: the agent keeps the layout to itself, and the test checks the
// host as a user would find it.
const (
	setupMarker       = "/var/lib/clowder-setup/done"
	renderedStackFile = "/var/lib/clowder/stacks/default_shop/compose.yaml"
	stackSecretsDir   = "/run/clowder-secrets/default_shop"
)

// The private registry that bin/dev live runs in the lab's network, with its
// files in the work directory.
const (
	registry       = labAddress + ":5000"
	registryUser   = "tester"
	privateImage   = registry + "/alpine:3.22"
	publicImage    = "alpine:3.22"
	registryCAFile = "registry/ca.crt"
	registryPWFile = "registry/password"
)

// shopStack is the test's stack with its volumes: app counts its starts on
// a replicated volume and reads a secret as a file and one as a variable;
// with a greeting, web joins it, with a healthcheck the test can fail.
// sleep as PID 1 ignores SIGTERM: the short grace period spares 10 s at
// each stop.
func shopStack(greeting string) string {
	spec := `services:
  app:
    image: ` + privateImage + `
    command: ["sh", "-c", "date >> /data/starts && exec sleep infinity"]
    stopGracePeriod: 1s
    volumes: [{volume: data, path: /data}, {volume: scratch, path: /scratch}]
    secrets: [password]
    secretEnv: {API_TOKEN: api-token}
`
	if greeting != "" {
		// the file comes back at every start: a restart keeps the
		// container's file system
		spec += `  web:
    image: ` + publicImage + `
    command: ["sh", "-c", "touch /tmp/healthy && exec sleep infinity"]
    stopGracePeriod: 1s
    env: {GREETING: ` + greeting + `}
    healthcheck:
      command: [test, -e, /tmp/healthy]
      interval: 2s
      retries: 2
`
	}
	return document(resource.KindStack, "shop", spec) +
		volumeDocument("data", "fast", "1G", 2) +
		volumeDocument("scratch", "bulk", "1G", 0)
}

// The deadlines of the steps. The setup installs packages, builds DRBD's
// module and may reboot into a newer kernel: about 5 minutes without a
// reboot, the deadline leaves room for two and a slow mirror.
const (
	setupDeadline     = 40 * time.Minute
	stackDeadline     = 10 * time.Minute
	rebootDeadline    = 10 * time.Minute
	unhealthyDeadline = 30 * time.Second
	// a pass of the agent after it started: the first one runs compose up
	// for every stack
	agentPass = 20 * time.Second
	// crashRestart is the goal for a crashed container to run again
	crashRestart = 2 * time.Second
	// agentMemoryLimit is the goal for the agent alone, ingressMemoryLimit
	// the goal for the agent and Traefik together
	agentMemoryLimit   = 150_000_000
	ingressMemoryLimit = 300_000_000
	manyStacks         = 20
)

func TestMain(m *testing.M) {
	if workDir == "" || clowderBinary == "" || os.Getenv("HLAB_CONTROL") == "" {
		fmt.Fprintln(os.Stderr, "set LIVE_DIR, CLOWDER and HLab's variables; bin/dev live does")
		os.Exit(2)
	}
	os.Exit(m.Run())
}

// step is a part of a scenario; the steps of one build on each other.
type step struct {
	name string
	run  func(t *testing.T)
}

// runSteps runs a scenario's steps as subtests; the first failure ends it.
func runSteps(t *testing.T, steps []step) {
	t.Helper()
	for _, step := range steps {
		if !t.Run(step.name, step.run) {
			return
		}
	}
}

// watchHost prints the host's state at the end of a test that failed.
func watchHost(t *testing.T, server host) {
	t.Cleanup(func() {
		if t.Failed() {
			diagnose(t, server)
		}
	})
}

// baselineConfigFile holds cluster.yaml as TestSetup applied it, without
// the values of its secrets, for baseline to compare.
const baselineConfigFile = "baseline-config.json"

// baseline checks where every scenario starts, so each runs alone and in any
// order (bin/dev live run -run <test>): the hosts that bin/dev live up made,
// its cluster set up with a leader and cluster.yaml applied as
// TestSetup left it, the first host active and the others drained (so
// stacks start on the first), three voters, no stack or volume in any
// namespace, and no fault in the lab. A scenario that fails leaves what it
// made for a look; the next one stops here and names it.
func baseline(t *testing.T) (host, outputs) {
	t.Helper()
	tofu := readOutputs(t)
	server := host{address: tofu.PublicAddress.Value}
	watchHost(t, server)
	want, err := os.ReadFile(filepath.Join(workDir, baselineConfigFile))
	if err != nil {
		t.Fatalf("the cluster is not set up (bin/dev live up does it): %v", err)
	}
	server.must(t, "test -e "+setupMarker+" && systemctl is-active clowderd")
	if status := clusterStatus(t); strings.Contains(status, "leader none") {
		t.Fatalf("the cluster has no leader:\n%s", status)
	}
	if got := clusterConfigJSON(t); !bytes.Equal(got, want) {
		t.Fatalf("an earlier scenario left cluster.yaml changed:\n%s\nwant, as TestSetup applied it:\n%s", got, want)
	}
	if diff := mustClowder(t, "cluster", "diff"); !strings.Contains(diff, "nothing to apply") {
		t.Fatalf("cluster.yaml is not applied:\n%s", diff)
	}
	hosts := clusterHosts(t)
	for _, found := range hosts {
		want := "draining"
		if found.ID == 0 {
			want = "active"
		}
		if found.Phase != want && found.Phase != "removed" {
			t.Fatalf("host %s is %s, want %s: %+v", found.Name, found.Phase, want, hosts)
		}
	}
	if err := checkThreeVoters(hosts); err != nil {
		t.Fatal(err)
	}
	// a deleted volume is listed until its host removed its copy
	eventually(t, time.Minute, "no stack or volume left", func() error {
		for _, kind := range []string{"stacks", "volumes"} {
			if left := resourcesInAllNamespaces(t, kind); len(left) > 0 {
				return fmt.Errorf("an earlier scenario left the %s %s; clowder delete removes them",
					kind, strings.Join(left, ", "))
			}
		}
		return nil
	})
	checkNoFaults(t)
	syncClocks(t, scenarioHosts(server, tofu))
	return server, tofu
}

// maxClockSkew is how far a host's clock may be off the test's. The hosts
// follow NTP servers on the internet, the test and the lab's ACME CA the
// clock of the Docker host, which may be off by a few seconds by itself.
const maxClockSkew = 10 * time.Second

// syncClocks brings the hosts' clocks back to the lab's: a VM that a
// scenario paused lags behind by the pause, and systemd-timesyncd polls
// only every half hour. A lagging clock takes a certificate the lab's ACME
// CA just issued as not yet valid. A restart of timesyncd syncs at once and
// steps the clock; it undoes what the lab's pause did, as a real host's NTP
// would in time.
func syncClocks(t *testing.T, hosts map[string]host) {
	t.Helper()
	skewOf := func(server host) (time.Duration, error) {
		output, err := server.run("date +%s.%N")
		if err != nil {
			return 0, err
		}
		seconds, err := strconv.ParseFloat(strings.TrimSpace(output), 64)
		if err != nil {
			return 0, err
		}
		return time.Since(time.Unix(0, int64(seconds*float64(time.Second)))).Abs(), nil
	}
	for name, server := range hosts {
		if skew, err := skewOf(server); err != nil || skew <= maxClockSkew {
			continue // a host that is not there has no clock to sync
		}
		server.must(t, "systemctl restart systemd-timesyncd")
		eventually(t, time.Minute, name+"'s clock in sync", func() error {
			skew, err := skewOf(server)
			if err == nil && skew > maxClockSkew {
				err = fmt.Errorf("off by %s", skew.Round(time.Millisecond))
			}
			return err
		})
	}
}

// resourcesInAllNamespaces lists a kind's resources as namespace/name.
func resourcesInAllNamespaces(t *testing.T, kind string) []string {
	t.Helper()
	var objects []resource.Object
	if err := json.Unmarshal([]byte(mustClowder(t, "get", kind, "-A", "-o", "json")), &objects); err != nil {
		t.Fatalf("read get %s: %v", kind, err)
	}
	var names []string
	for _, object := range objects {
		names = append(names, object.Metadata.Namespace+"/"+object.Metadata.Name)
	}
	return names
}

// TestSetup makes the first host, sets up its cluster and has two more
// hosts join: the baseline of the other scenarios. bin/dev live up runs it,
// once per cluster.
func TestSetup(t *testing.T) {
	tofu := readOutputs(t)
	server := host{address: tofu.PublicAddress.Value}
	watchHost(t, server)
	runSteps(t, []step{
		{"the setup makes the first host", func(t *testing.T) { waitForSetup(t, server) }},
		{"the user gets credentials over SSH and applies the cluster", func(t *testing.T) { setUpCluster(t, server, tofu) }},
		{"two hosts join with a token each", growToThreeHosts},
		{"each host starts fresh, with a DRBD module for its kernel", hostsStartFresh},
	})
}

// TestStacks runs a stack's way from the first apply through restarts, a
// reboot and changed secrets to its delete.
func TestStacks(t *testing.T) {
	server, tofu := baseline(t)
	runSteps(t, []step{
		{"a stack runs from a private registry with volumes and secrets", func(t *testing.T) { runStack(t, server) }},
		{"the same files again change nothing", func(t *testing.T) { applyAgainChangesNothing(t, server) }},
		{"a dry run makes nothing", func(t *testing.T) { dryRunMakesNothing(t, server) }},
		{"lost credentials come back from root on the host", func(t *testing.T) { recoverAccess(t, server) }},
		{"the recovery key changes", func(t *testing.T) { rotateRecoveryKey(t, server) }},
		{"a restart recreates the stack, and the watch sees it", func(t *testing.T) {
			restartRecreatesAndTheWatchSeesIt(t, server)
		}},
		{"namespaces keep their stacks apart, a volume outlives its stack", func(t *testing.T) {
			namespacesKeepTheirStacksApart(t, server)
		}},
		{"an agent restart leaves the containers alone", func(t *testing.T) { restartAgent(t, server) }},
		{"the supervisor restarts what crashed and nothing else", func(t *testing.T) { supervise(t, server) }},
		{"the stack comes back after a reboot", func(t *testing.T) { reboot(t, server, tofu) }},
		{"a changed secret recreates its container only", func(t *testing.T) { changeSecrets(t, server) }},
		{"compose fails on a missing secret file", func(t *testing.T) { missingSecretFile(t, server) }},
		{"a volume that fits nowhere waits with its reason", func(t *testing.T) { fullPool(t, server) }},
		{"twenty stacks run", func(t *testing.T) { runManyStacks(t, server) }},
		{"the stack is deleted with its volumes", func(t *testing.T) { deleteStack(t, server) }},
	})
}

// hostsStartFresh checks that each host, made from Debian's image or from
// the prepared one, is a server of its own: cloud-init ran as for a new
// instance, and the machine-id and the SSH host key are its own. DRBD's
// module, built when the image was prepared, fits the running kernel.
func hostsStartFresh(t *testing.T) {
	tofu := readOutputs(t)
	seen := map[string]string{} // what a host has -> the host that has it
	for name, server := range scenarioHosts(host{address: tofu.PublicAddress.Value}, tofu) {
		identity := server.must(t, "cloud-init status --wait >/dev/null; cloud-init query instance_id; "+
			"cat /etc/machine-id; ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub")
		for _, line := range strings.Split(identity, "\n") {
			if other, ok := seen[line]; ok {
				t.Errorf("%s and %s share %q: the image was not cleaned for reuse", name, other, line)
			}
			seen[line] = name
		}
		if fits := server.must(t, `test "$(modinfo -F vermagic drbd | cut -d ' ' -f 1)" = "$(uname -r)" && echo yes || echo no`); fits != "yes" {
			t.Errorf("%s: DRBD's module does not fit the running kernel %s", name, server.must(t, "uname -r"))
		}
	}
}

// waitForSetup waits until clowder-setup has made the host the first of a
// cluster and the agent runs.
func waitForSetup(t *testing.T, server host) {
	eventually(t, setupDeadline, "clowder-setup done", server.setupReached(setupMarker))
	server.must(t, "systemctl is-active clowderd")
	checkMemory(t, server, "idle")
}

// setUpCluster gives the test the CLI's first credentials from root on the
// first host, as over SSH, and the recovery key in a file; then it fills in
// cluster.yaml from the module's outputs, with the Hetzner token and the
// registry's login encrypted, and applies it.
func setUpCluster(t *testing.T, server host, tofu outputs) {
	// gone before, so a step run again starts as the first time: import
	// refuses a context that is there, --recovery-out a file
	for _, path := range []string{credentialsFile(), filepath.Join(workDir, "recovery-key")} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(clusterDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	// stdout alone: must's output has the command's stderr too
	issued := server.must(t, "clowderd credentials 2>/dev/null")
	if _, err := clowderWithInput(strings.NewReader(issued), "config", "import", "-"); err != nil {
		t.Fatal(err)
	}
	mustClowder(t, "recovery", "rotate", "--recovery-out", filepath.Join(workDir, "recovery-key"))

	// the module's part of cluster.yaml, read strictly into the real type, so a
	// renamed field fails here and not only in the cluster
	fromModule, err := yaml.Marshal(tofu.ClusterYAML.Value)
	if err != nil {
		t.Fatal(err)
	}
	var config clusterspec.Config
	decoder := yaml.NewDecoder(bytes.NewReader(fromModule))
	decoder.KnownFields(true)
	if err := decoder.Decode(&config); err != nil {
		t.Fatalf("the module's cluster_yaml: %v", err)
	}
	config.Hetzner = clusterspec.Hetzner{Token: "hetzner.token", API: os.Getenv("HCLOUD_ENDPOINT")}
	password := pushPrivateImage(t, server)
	registryPassword := yamlpath.Key(yamlpath.Key("registries", registry), "password")
	config.Registries = map[string]clusterspec.Registry{
		registry: {Username: registryUser, Password: clusterspec.SecretRef(registryPassword)},
	}
	setClusterSecrets(t, map[string]string{"hetzner.token": os.Getenv("HCLOUD_TOKEN"), registryPassword: password})
	writeConfig(t, config, nil)
	checkNothingPersonal(t, os.Getenv("HCLOUD_TOKEN"), password)

	// HLab's API is plain http: the CLI warns about it, on stderr before the diff
	httpWarning := "warning: hetzner.api is set: clowder uses " + os.Getenv("HCLOUD_ENDPOINT") +
		" instead of Hetzner's own API, which is meant for test labs; over http the Hetzner token crosses the network unencrypted\n"
	if diff := mustClowder(t, "cluster", "diff"); !strings.Contains(diff, "never applied") || !strings.HasPrefix(diff, httpWarning) {
		t.Errorf("diff before the first apply:\n%s", diff)
	}
	mustClowder(t, "cluster", "apply")
	// another token is named, its value not
	setClusterSecrets(t, map[string]string{"hetzner.token": "another token"})
	writeConfig(t, config, nil)
	if diff := mustClowder(t, "cluster", "diff"); diff != httpWarning+"the secret hetzner.token differs\n" {
		t.Errorf("diff after the token changed:\n%s", diff)
	}
	// encrypted anew, the same token is no change
	setClusterSecrets(t, map[string]string{"hetzner.token": os.Getenv("HCLOUD_TOKEN")})
	writeConfig(t, config, nil)
	if diff := mustClowder(t, "cluster", "diff"); !strings.Contains(diff, "nothing to apply") {
		t.Errorf("diff after the token was encrypted anew:\n%s", diff)
	}
	status := mustClowder(t, "cluster", "status")
	if strings.Contains(status, "leader none") {
		t.Errorf("the cluster has no leader:\n%s", status)
	}
}

// checkNothingPersonal checks what the setup leaves where a repository
// would hold it: cluster.yaml alone in the cluster directory, none of the
// values in clear; the credentials are in the user's own file, for the
// user alone.
func checkNothingPersonal(t *testing.T, values ...string) {
	t.Helper()
	entries, err := os.ReadDir(clusterDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != clusterdir.ConfigFile {
			t.Errorf("the cluster directory has %s", entry.Name())
		}
	}
	config, err := os.ReadFile(filepath.Join(clusterDir(), clusterdir.ConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	for index, value := range values {
		if strings.Contains(string(config), value) {
			t.Errorf("cluster.yaml has value %d in clear", index)
		}
	}
	credentials, err := os.Stat(credentialsFile())
	if err != nil {
		t.Fatal(err)
	}
	if mode := credentials.Mode().Perm(); mode != 0o600 {
		t.Errorf("the credentials have the mode %o, want 600", mode)
	}
}

// pushPrivateImage makes the host trust the registry (Docker's trust per
// registry; daemon.json stays as it is) and puts the test's image there.
// Then it logs out and removes the image, so only the agent's own login
// (the same /root/.docker of root) can pull it. It returns the password.
func pushPrivateImage(t *testing.T, server host) string {
	server.upload(t, filepath.Join(workDir, registryCAFile), "/etc/docker/certs.d/"+registry+"/ca.crt")
	password, err := os.ReadFile(filepath.Join(workDir, registryPWFile))
	if err != nil {
		t.Fatal(err)
	}
	login := fmt.Sprintf("docker login --username %s --password-stdin %s", registryUser, registry)
	server.must(t, "docker pull -q "+publicImage)
	if _, err := server.runWithInput(login, bytes.NewReader(password)); err != nil {
		t.Fatal(err)
	}
	server.must(t, fmt.Sprintf("docker tag %s %s && docker push -q %s", publicImage, privateImage, privateImage))
	server.must(t, fmt.Sprintf("docker logout %s && docker rmi %s", registry, privateImage))
	return strings.TrimSpace(string(password))
}

// The stack's secrets in each generation; a $ must reach the container as
// it is.
var (
	firstSecrets  = map[string]string{"password": "first pa$$word", "api-token": "tok$en 'one'"}
	secondSecrets = map[string]string{"password": "second pa$$word", "api-token": "tok$en 'two'"}
)

// applyShop writes the stack's resources and secrets and applies them, each
// secret encrypted anew (secret encrypt): password in the stack's file,
// api-token on stdin. Then it waits until the stack's generation runs with
// these secrets.
func applyShop(t *testing.T, greeting string, secrets map[string]string) {
	t.Helper()
	if err := os.MkdirAll(stackDir("shop"), 0o755); err != nil {
		t.Fatal(err)
	}
	before := stackInfo(t, "shop").generation // 0 before the first apply
	digestsBefore := secretDigests(t)
	// the file starts with the stack and its volumes; the password after
	// them is encrypted anew each time
	previous, _ := os.ReadFile(stackFile("shop"))
	specChanged := before == 0 || !strings.HasPrefix(string(previous), shopStack(greeting))
	password := encryptSecret(t, "password", secrets["password"])
	if err := os.WriteFile(stackFile("shop"), []byte(shopStack(greeting)+"---\n"+password), 0o644); err != nil {
		t.Fatal(err)
	}
	token := encryptSecret(t, "api-token", secrets["api-token"])
	output, err := clowderWithInput(strings.NewReader(token), "apply", "-f", "-")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("clowder apply -f - (api-token):\n%s", output)
	applied := appliedSecrets(output)
	// the secrets before the stack, as apply orders a directory
	output = mustClowder(t, "apply", "-f", stackDir("shop"))
	applied = append(applied, appliedSecrets(output)...)
	// a changed spec is the stack's next generation; a change of secrets
	// alone keeps it
	want := before
	if specChanged {
		want++
	}
	// each clowder call is a client of its own, and reads come from the
	// copy of the state of the host the CLI asks, which may be a follower's a moment behind
	// the write; the secrets come before the stack in the log, so they are
	// there with it
	var generation int64
	eventually(t, time.Minute, "the apply in the asked host's state", func() error {
		if generation = stackInfo(t, "shop").generation; generation < want {
			return fmt.Errorf("generation %d", generation)
		}
		return nil
	})
	if generation != want {
		t.Errorf("generation %d after the apply, want %d", generation, want)
	}
	waitRunning(t, "shop", generation)
	// a change of secrets alone keeps the generation, so the waits above
	// pass on a seed's copy from before the apply: wait until it has the
	// changed secrets too
	var digests map[string]string
	eventually(t, time.Minute, "the changed secrets in the seed's state", func() error {
		digests = secretDigests(t)
		for _, name := range applied {
			if digests[name] == digestsBefore[name] {
				return fmt.Errorf("secret %s has its old digest", name)
			}
		}
		return nil
	})
	waitSecrets(t, "shop", map[string]string{"password": digests["password"], "api-token": digests["api-token"]})
}

// appliedSecrets are the secrets that an apply's output names as applied,
// changed or new; an unchanged one keeps its digest.
func appliedSecrets(output string) []string {
	var names []string
	for _, line := range strings.Split(output, "\n") {
		if name, ok := strings.CutPrefix(strings.TrimSpace(line), "Secret default/"); ok {
			if name, ok = strings.CutSuffix(name, ": applied"); ok {
				names = append(names, name)
			}
		}
	}
	return names
}

// runStack applies the stack and checks the container: its image from the
// private registry, its secrets, its volume, both pools.
func runStack(t *testing.T, server host) {
	refuseSecretsOutOfPlace(t)
	applyShop(t, "", firstSecrets)
	mustClowder(t, "stack", "ps", "shop")

	app := server.service(t, "app")
	if image := server.must(t, "docker inspect --format '{{.Config.Image}}' "+app); image != privateImage {
		t.Errorf("app runs %s, want %s", image, privateImage)
	}
	checkSecrets(t, server, firstSecrets)
	if got := starts(t, server); got != 1 {
		t.Errorf("app started %d times on its volume, want 1", got)
	}
	// data (replicated) went to the pool of fast, scratch to the one of bulk
	data, scratch := volumeLV(t, "data"), volumeLV(t, "scratch")
	server.must(t, "lvs --noheadings -o lv_name vg-fast | grep -qw "+data)
	server.must(t, "lvs --noheadings -o lv_name vg-bulk | grep -qw "+scratch)
	server.must(t, "drbdsetup status | grep -qw "+data)
	rekeyAndDecrypt(t, server, filepath.Join(workDir, "recovery-key"))
}

// rekeyAndDecrypt encrypts the values of the stack's files and of
// cluster.yaml anew through the cluster, which changes nothing the cluster
// keeps and recreates no container, and reads a value back with the
// recovery key, as in an emergency.
func rekeyAndDecrypt(t *testing.T, server host, recoveryKey string) {
	t.Helper()
	before := shopContainers(t, server)
	mustClowder(t, "secret", "rekey", "-f", stackDir("shop"))
	mustClowder(t, "secret", "rekey", "-f", clusterDir())
	if diff := mustClowder(t, "cluster", "diff"); !strings.Contains(diff, "nothing to apply") {
		t.Errorf("cluster.yaml encrypted anew:\n%s", diff)
	}
	if out := mustClowder(t, "apply", "-f", stackDir("shop")); !strings.Contains(out, "Secret default/password: unchanged") {
		t.Errorf("the stack's files encrypted anew:\n%s", out)
	}
	checkUnchanged(t, before, shopContainers(t, server))
	out := mustClowder(t, "secret", "decrypt", "--identity", recoveryKey, "-f", stackFile("shop"))
	if want := "default/password\t" + firstSecrets["password"] + "\n"; out != want {
		t.Errorf("decrypt: %q, want %q", out, want)
	}
}

// refuseSecretsOutOfPlace applies what the cluster must refuse: a Secret in
// clear, in a file and on stdin, and a value of cluster.yaml (the Hetzner
// token) copied into a Secret, which a container would then read.
func refuseSecretsOutOfPlace(t *testing.T) {
	t.Helper()
	clear := "apiVersion: clowder/v1\nkind: Secret\nmetadata: {name: copied}\nspec: {value: s3cret}\n"
	clearFile := filepath.Join(workDir, "clear.yaml")
	if err := os.WriteFile(clearFile, []byte(clear), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := clowder("apply", "-f", clearFile); err == nil || !strings.Contains(err.Error(), "write its value encrypted") {
		t.Errorf("a Secret in clear in a file: %v", err)
	}
	if _, err := clowderWithInput(strings.NewReader(clear), "apply", "-f", "-"); err == nil ||
		!strings.Contains(err.Error(), "write its value encrypted") {
		t.Errorf("a Secret in clear on stdin: %v", err)
	}
	block, err := clowderWithInput(strings.NewReader("the Hetzner token"), "secret", "encrypt", "--cluster-yaml")
	if err != nil {
		t.Fatal(err)
	}
	copied := "apiVersion: clowder/v1\nkind: Secret\nmetadata: {name: copied}\nspec:\n  value: " + block
	if output, err := clowderWithInput(strings.NewReader(copied), "apply", "-f", "-"); err == nil ||
		!strings.Contains(output, "the value is for cluster, not for default/copied") {
		t.Errorf("a value of cluster.yaml as a Secret: %v\n%s", err, output)
	}
}

func checkSecrets(t *testing.T, server host, want map[string]string) {
	t.Helper()
	app := server.service(t, "app")
	if got := server.must(t, "docker exec "+app+" cat /run/secrets/password"); got != want["password"] {
		t.Errorf("/run/secrets/password = %q, want %q", got, want["password"])
	}
	if got := server.must(t, "docker exec "+app+" printenv API_TOKEN"); got != want["api-token"] {
		t.Errorf("API_TOKEN = %q, want %q", got, want["api-token"])
	}
}

// starts is how often app started on its volume: the data stays across
// reboots and new generations.
func starts(t *testing.T, server host) int {
	t.Helper()
	count, err := strconv.Atoi(server.must(t, "docker exec "+server.service(t, "app")+
		" sh -c 'wc -l < /data/starts'"))
	if err != nil {
		t.Fatal(err)
	}
	return count
}

// shopContainers are the containers of the stack by ID, with their state.
func shopContainers(t *testing.T, server host) map[string]container {
	t.Helper()
	containers := map[string]container{}
	for _, id := range strings.Fields(server.must(t,
		"docker ps -aq --no-trunc --filter label=com.docker.compose.project="+project("shop"))) {
		containers[id] = server.inspect(t, id)
	}
	return containers
}

// restartAgent restarts clowderd: with live-restore the containers run on,
// and its first pass, which runs compose up for every stack, changes
// nothing that runs. Docker's RestartCount would say nothing: the
// supervisor starts containers through the API.
func restartAgent(t *testing.T, server host) {
	before := shopContainers(t, server)
	wanted := stackInfo(t, "shop").generation
	server.must(t, "systemctl restart clowderd")
	waitRunning(t, "shop", wanted)
	time.Sleep(agentPass)
	checkUnchanged(t, before, shopContainers(t, server))
}

// checkUnchanged fails unless the same containers run, none restarted.
func checkUnchanged(t *testing.T, before, after map[string]container) {
	t.Helper()
	if len(after) != len(before) {
		t.Fatalf("%d containers before, %d after", len(before), len(after))
	}
	for id, was := range before {
		if now, ok := after[id]; !ok || !now.State.StartedAt.Equal(was.State.StartedAt) {
			t.Errorf("container %.12s was replaced or restarted", id)
		}
	}
}

// supervise adds web, then crashes app, makes web unhealthy and updates
// web alone.
func supervise(t *testing.T, server host) {
	appBefore := server.inspect(t, server.service(t, "app"))
	applyShop(t, "one", firstSecrets)
	app := server.inspect(t, server.service(t, "app"))
	if app.ID != appBefore.ID {
		t.Error("adding web recreated app")
	}

	// kill -9 of its process, not docker kill: the supervisor takes a
	// docker kill with SIGKILL as a stop by hand and keeps it stopped
	server.must(t, "kill -9 "+strconv.Itoa(app.State.Pid))
	eventually(t, stackDeadline, "app runs again", func() error {
		if now := server.inspect(t, app.ID); !now.State.StartedAt.After(app.State.StartedAt) || now.State.Pid == 0 {
			return fmt.Errorf("not yet")
		}
		return nil
	})
	crashed := server.inspect(t, app.ID)
	gap := crashed.State.StartedAt.Sub(crashed.State.FinishedAt)
	t.Logf("app ran again %s after its crash", gap)
	if gap > crashRestart {
		t.Errorf("app ran again %s after its crash, want at most %s", gap, crashRestart)
	}

	web := server.inspect(t, server.service(t, "web"))
	server.must(t, "docker exec "+web.ID+" rm /tmp/healthy")
	eventually(t, unhealthyDeadline, "web restarted and healthy", func() error {
		now := server.inspect(t, web.ID)
		if !now.State.StartedAt.After(web.State.StartedAt) || now.State.Health == nil || now.State.Health.Status != "healthy" {
			return fmt.Errorf("started %s, health %+v", now.State.StartedAt, now.State.Health)
		}
		return nil
	})

	// only web changes: app stays as it is, web comes up new exactly once
	appNow := server.inspect(t, app.ID)
	since := server.must(t, "date +%s")
	applyShop(t, "two", firstSecrets)
	if now := server.inspect(t, server.service(t, "app")); now.ID != app.ID ||
		!now.State.StartedAt.Equal(appNow.State.StartedAt) {
		t.Error("an update of web alone restarted or recreated app")
	}
	newWeb := server.service(t, "web")
	if newWeb == web.ID {
		t.Fatal("web was not recreated with its new environment")
	}
	// whole seconds, from before the apply to the next second (until the
	// current one would miss a start in this second); the old web's starts
	// are not counted, as the filter takes the new container only
	started := server.must(t, fmt.Sprintf("docker events --since %s --until $(($(date +%%s) + 1)) "+
		"--filter container=%s --filter event=start --format '{{.ID}}' | wc -l", since, newWeb))
	if started != "1" {
		t.Errorf("the new web started %s times during the update, want 1", started)
	}
}

// reboot restarts the host: after the boot, the agent brings the stack back,
// no container started before the agent, the operator base (the Floating IP,
// the pools, the watchdog) is still there, and the mount guard holds.
func reboot(t *testing.T, server host, tofu outputs) {
	startsBefore := starts(t, server)
	wanted := stackInfo(t, "shop").generation
	// without its marker the setup runs again at this boot, as after its own
	// reboot into a newer kernel: systemd must start it, and each of its
	// steps must hold on a finished host
	server.must(t, "rm "+setupMarker)
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
	eventually(t, setupDeadline, "clowder-setup finished again", func() error {
		_, err := server.run("test -e " + setupMarker)
		return err
	})
	// active only if it ran in this boot (RemainAfterExit)
	server.must(t, "systemctl is-active clowder-setup")
	waitRunning(t, "shop", wanted)
	eventually(t, stackDeadline, "the stack's containers run", func() error {
		if server.service(t, "app") == "" || server.service(t, "web") == "" {
			return fmt.Errorf("not yet")
		}
		return nil
	})
	checkSecrets(t, server, firstSecrets)
	if got := starts(t, server); got != startsBefore+1 {
		t.Errorf("app started %d times on its volume, want %d", got, startsBefore+1)
	}
	checkStartOrder(t, server)

	classes, ok := tofu.ClusterYAML.Value["storage_classes"].(map[string]any)
	if !ok {
		t.Fatalf("the module's cluster_yaml has no storage_classes")
	}
	var pools []string
	for class := range classes {
		pools = append(pools, "--pool vg-"+class+"/thin")
	}
	// the time sync comes a while after the boot; the rest must hold at once
	eventually(t, rebootDeadline, "the time is synchronized", func() error {
		_, err := server.run("timedatectl show --property=NTPSynchronized --value | grep -qx yes")
		return err
	})
	server.must(t, "clowderd check "+strings.Join(pools, " ")+" --floating-ip "+tofu.FloatingIP.Value)
	checkMountGuard(t, server)
}

// checkStartOrder: Docker restarts nothing itself (restart "no"), so every
// container started after clowderd's first start in this boot. That start,
// not ExecMainStartTimestamp, which a later restart of the agent (watchdog,
// the setup) would move; and not ActiveEnterTimestamp: the agent runs
// compose up before it tells systemd it is ready.
func checkStartOrder(t *testing.T, server host) {
	agentStart := unixTime(t, strings.Fields(server.must(t,
		"journalctl --boot --no-pager -o short-unix -u clowderd | grep -m 1 'Starting clowderd'"))[0])
	t.Logf("clowderd started at %s; restarts in this boot: %s", agentStart,
		server.must(t, "systemctl show clowderd --property=NRestarts --value"))
	for id, state := range shopContainers(t, server) {
		if policy := state.HostConfig.RestartPolicy.Name; policy != "no" {
			t.Errorf("container %.12s has the restart policy %q", id, policy)
		}
		if !state.State.StartedAt.After(agentStart) {
			t.Errorf("container %.12s started at %s, before the agent at %s", id, state.State.StartedAt, agentStart)
		}
	}
}

// checkMountGuard unmounts app's replicated volume while the agent and app
// are stopped: app must not start, and nothing may land in the empty mount
// point on the root file system. Started again, the agent mounts the volume
// and brings app back with its data. scratch stays mounted; one volume
// shows the guard.
func checkMountGuard(t *testing.T, server host) {
	startsBefore := starts(t, server)
	app := server.service(t, "app")
	device := server.must(t, "docker volume inspect --format '{{index .Options \"device\"}}' "+volumeLV(t, "data"))
	if path.Base(device) != "data" {
		t.Fatalf("the volume binds %s, not the data directory of a mount", device)
	}
	mount := path.Dir(device)
	server.must(t, "systemctl stop clowderd && docker stop "+app+" && umount "+mount)
	if output, err := server.run("docker start " + app); err == nil {
		t.Errorf("app started on its unmounted volume:\n%s", output)
	}
	if _, err := server.run("test -e " + device); err == nil {
		t.Errorf("%s exists on the root file system", device)
	}
	server.must(t, "systemctl start clowderd")
	eventually(t, stackDeadline, "the agent brought app back", func() error {
		if server.service(t, "app") == "" {
			return fmt.Errorf("app does not run")
		}
		return nil
	})
	if got := starts(t, server); got != startsBefore+1 {
		t.Errorf("app started %d times on its volume, want %d", got, startsBefore+1)
	}
}

// changeSecrets applies the stack with new secrets: compose recreates app,
// whose secret file changed (its label), and leaves web, which has none.
func changeSecrets(t *testing.T, server host) {
	app, web := server.service(t, "app"), server.service(t, "web")
	startsBefore := starts(t, server)
	applyShop(t, "two", secondSecrets)
	if server.service(t, "app") == app {
		t.Error("app was not recreated with its new secrets")
	}
	if server.service(t, "web") != web {
		t.Error("web, which asks for no secret, was recreated")
	}
	checkSecrets(t, server, secondSecrets)
	// the two secrets come in two applies (stdin, then the directory): a
	// pass of the agent between them recreates app once for each
	if got := starts(t, server); got != startsBefore+1 && got != startsBefore+2 {
		t.Errorf("app started %d times on its volume, want %d or %d", got, startsBefore+1, startsBefore+2)
	}
}

// missingSecretFile runs compose up with the agent's files and a secret file
// gone: compose must fail rather than start the container without the
// secret, which the agent relies on (docs/developing/host.md, The agent). The agent is
// stopped meanwhile, so neither it nor its supervisor acts on what compose
// does; the containers keep running (KillMode=process). Started again, the
// agent writes the secrets anew and brings the stack up as after a boot.
func missingSecretFile(t *testing.T, server host) {
	server.must(t, "systemctl stop clowderd")
	server.must(t, "rm "+stackSecretsDir+"/files/password")
	output, err := server.run("docker compose --project-name " + project("shop") + " --file " + renderedStackFile +
		" --file " + stackSecretsDir + "/env.yaml up --detach --force-recreate")
	if err == nil {
		t.Errorf("compose up started the stack without its secret file:\n%s", output)
	} else {
		t.Logf("compose up without the secret file failed as it should:\n%s", output)
	}
	server.must(t, "systemctl start clowderd")
	// the failed recreate leaves old containers renamed
	// (<id>_default_shop_web); the agent's compose up replaces or starts
	// them (Compose may keep such a name), so wait until each service has one
	// container, running
	eventually(t, stackDeadline, "the agent brought the stack back", func() error {
		containers := strings.Fields(server.must(t, "docker ps -a --filter label=com.docker.compose.project="+project("shop")+" "+
			"--format '{{.Label \"com.docker.compose.service\"}}:{{.State}}' | sort"))
		if !slices.Equal(containers, []string{"app:running", "web:running"}) {
			return fmt.Errorf("containers %v", containers)
		}
		return nil
	})
	// the agent's pass may still replace a container right after: the
	// next step compares the containers, so they must stay as they are
	// for a whole pass
	eventually(t, stackDeadline, "the stack's containers stay as they are", func() error {
		listed := func() string {
			return server.must(t, "docker ps -a --no-trunc --filter label=com.docker.compose.project="+project("shop")+
				" --format '{{.ID}} {{.State}}' | sort")
		}
		before := listed()
		time.Sleep(agentPass)
		if after := listed(); after != before {
			return fmt.Errorf("they changed from %q to %q", before, after)
		}
		return nil
	})
	checkSecrets(t, server, secondSecrets)
}

// fullPool applies a stack whose volume fits no pool: it waits, the cluster
// says why, and the running stack is untouched.
func fullPool(t *testing.T, server host) {
	before := shopContainers(t, server)
	createAndApply(t, "big", simpleStack("big", "huge")+volumeDocument("huge", "bulk", "1T", 0))
	eventually(t, stackDeadline, "big waits with a reason", func() error {
		stacks, err := listStacks()
		if err != nil {
			return err
		}
		big := stacks["big"]
		if big.Status != "pending" || !strings.Contains(big.WaitReason, "volume huge (1T, class bulk) fits no pool") {
			return fmt.Errorf("%s, reason %q", big.Status, big.WaitReason)
		}
		return nil
	})
	mustClowder(t, "get", "stacks")
	checkUnchanged(t, before, shopContainers(t, server))
	removeStack(t, "big")
}

// simpleStack is a stack of one service on the public image that mounts
// the volume of the namespace at /data, if one is named. It only sleeps, so
// it reserves little: with the default (0.1 CPUs) the host's 2 CPUs would
// take 17 services (2 CPUs less the system reserve of 0.25, in tenths),
// fewer than shop's two and the twenty stacks.
func simpleStack(name, volume string) string {
	spec := "services:\n  app:\n    image: " + publicImage + "\n    command: [sleep, infinity]\n" +
		"    stopGracePeriod: 1s\n    resources: {reserve: {cpus: 0.01, memory: 16M}}\n"
	if volume != "" {
		spec += "    volumes: [{volume: " + volume + ", path: /data}]\n"
	}
	return document(resource.KindStack, name, spec)
}

// runManyStacks runs twenty small stacks next to shop, measures the agent
// and removes them again.
func runManyStacks(t *testing.T, server host) {
	var names []string
	for i := 1; i <= manyStacks; i++ {
		names = append(names, fmt.Sprintf("s%02d", i))
	}
	for _, name := range names {
		createAndApply(t, name, simpleStack(name, ""))
	}
	eventually(t, stackDeadline, "all stacks run", func() error {
		stacks, err := listStacks()
		if err != nil {
			return err
		}
		for _, name := range names {
			if stacks[name].Status != "running" {
				return fmt.Errorf("%s is %q", name, stacks[name].Status)
			}
		}
		return nil
	})
	checkMemory(t, server, fmt.Sprintf("with %d stacks", manyStacks+1))
	for _, name := range names {
		mustClowder(t, "delete", "stack", name)
	}
	waitGone(t, names...)
}

// deleteStack deletes the stack, then its volumes and secrets: the
// containers, the LVs and the DRBD resource go. The delete of the stack's
// directory takes the stack before its volumes and the password; api-token
// came on stdin.
func deleteStack(t *testing.T, server host) {
	data, scratch := volumeLV(t, "data"), volumeLV(t, "scratch")
	mustClowder(t, "delete", "--with-data", "-f", stackDir("shop"))
	mustClowder(t, "delete", "secret", "api-token")
	waitGone(t, "shop")
	eventually(t, stackDeadline, "the host removed the stack", func() error {
		output, err := server.run("docker ps -aq --filter label=com.docker.compose.project=" + project("shop") + "; " +
			"lvs --noheadings -o lv_name | grep -w -e " + data + " -e " + scratch + "; " +
			"drbdsetup status | grep -w " + data + "; true")
		if err == nil && output != "" {
			return fmt.Errorf("left on the host:\n%s", output)
		}
		return err
	})
}

// checkMemory logs what the agent and Traefik use and fails above the
// goals, so a regression shows. Before the first cluster apply Traefik does
// not run yet.
func checkMemory(t *testing.T, server host, when string) {
	t.Helper()
	agent := residentKiB(t, server, "$(systemctl show clowderd --property=MainPID --value)")
	t.Logf("clowderd %s: resident %d KiB, its cgroup %s bytes", when, agent,
		server.must(t, "systemctl show clowderd --property=MemoryCurrent --value"))
	if agent<<10 > agentMemoryLimit {
		t.Errorf("clowderd uses %d KiB, more than the goal of %d MB", agent, agentMemoryLimit/1_000_000)
	}
	traefik := server.must(t, "docker ps -q --filter label=com.docker.compose.project=clowder-traefik")
	if traefik == "" {
		return
	}
	proxy := residentKiB(t, server, "$(docker inspect --format '{{.State.Pid}}' "+traefik+")")
	t.Logf("Traefik %s: resident %d KiB; with clowderd %d KiB", when, proxy, agent+proxy)
	if (agent+proxy)<<10 > ingressMemoryLimit {
		t.Errorf("clowderd and Traefik use %d KiB, more than the goal of %d MB", agent+proxy, ingressMemoryLimit/1_000_000)
	}
}

// residentKiB is a process's resident memory; pid is a shell expression.
func residentKiB(t *testing.T, server host, pid string) int64 {
	t.Helper()
	resident, err := strconv.ParseInt(server.must(t, "ps -o rss= -p "+pid), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return resident
}
