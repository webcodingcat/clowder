//go:build live

package live

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/webcodingcat/clowder/internal/api"
	"github.com/webcodingcat/clowder/internal/buildinfo"
	"github.com/webcodingcat/clowder/internal/clientconfig"
	"github.com/webcodingcat/clowder/internal/hostrole"
	"github.com/webcodingcat/clowder/internal/jointoken"
	"github.com/webcodingcat/clowder/internal/resource"
)

// The host's own certificate and its key, as the agent keeps them
// (agent.Files).
const (
	hostCertificateFile = "/var/lib/clowder/pki/host.pem"
	hostKeyFile         = "/var/lib/clowder/pki/host-key.pem"
)

// TestRoles checks the layout roles: the CLI reaches the control plane
// only, a volume's copies with a disk go on the workers and its tiebreaker
// on the control plane, and a worker gets no further than its own part: its
// token makes no control-plane host, the control plane's routes and Raft
// refuse its certificate, its own API refuses the CLI, and it holds no data
// key and gets only the secrets of its stacks.
func TestRoles(t *testing.T) {
	server, tofu := baseline(t, splitRoles)
	hosts := scenarioHosts(server, tofu)
	layout := labLayout(t)
	runSteps(t, []step{
		{"the CLI reaches the control plane only", func(t *testing.T) { workersCloseTheirAPI(t, tofu, layout) }},
		{"a volume has its copies on the workers and its tiebreaker on the control plane", func(t *testing.T) {
			createAndApply(t, "spread", volumeDocument("spread", "fast", failoverVolumeSize, 2)+simpleStack("spread", "spread"))
			waitRunning(t, "spread", 1)
			volumeSteady(t, hosts, "spread")
			checkCopiesOnWorkers(t, layout, "spread")
		}},
		{"a worker's token makes no control-plane host", func(t *testing.T) { workerTokenRefusedForControlPlane(t, tofu) }},
		{"a worker is refused on the control plane's routes and on Raft", func(t *testing.T) {
			workerRefusedByControlPlane(t, hosts)
		}},
		{"a worker refuses the CLI", func(t *testing.T) { workerRefusesTheCLI(t, hosts, tofu) }},
		{"a worker gets only the secrets of its stacks", func(t *testing.T) {
			workerGetsOnlyItsSecrets(t, hosts, layout)
		}},
		{"the stack and its volume go", func(t *testing.T) { removeStack(t, "spread") }},
	})
}

// TestRolesFailover fails the worker of a stack: the other worker takes the
// stack with its data, and the failed one comes back.
func TestRolesFailover(t *testing.T) {
	server, tofu := baseline(t, splitRoles)
	hosts := scenarioHosts(server, tofu)
	layout := labLayout(t)
	cleanUpFaults(t)
	runSteps(t, []step{
		{"keeper has its copies on the workers", func(t *testing.T) {
			createAndApply(t, "keeper", volumeDocument("keeper", "fast", failoverVolumeSize, 2)+simpleStack("keeper", "keeper"))
			waitRunning(t, "keeper", 1)
			volumeSteady(t, hosts, "keeper")
			checkCopiesOnWorkers(t, layout, "keeper")
		}},
		{"its worker freezes, and the other worker runs it", func(t *testing.T) {
			target := stackInfo(t, "keeper").Host
			writeProof(t, hosts[target], "keeper", "roles")
			start := time.Now()
			labControl(t, "/vm/"+target+"/pause", nil)
			failsOver(t, hosts, "keeper", target, start, "roles")
			if on := stackInfo(t, "keeper").Host; !layout.hasRole(on, "worker") {
				t.Errorf("keeper failed over to %s, which is no worker", on)
			}
			waitBack(t, hosts, target, "keeper")
			volumeSteady(t, hosts, "keeper")
			checkCopiesOnWorkers(t, layout, "keeper")
		}},
		{"the stack and its volume go", func(t *testing.T) { removeStack(t, "keeper") }},
	})
}

// workersCloseTheirAPI checks that the control-plane hosts answer on the
// API's public port and the workers do not: the module opens the port only
// on the control plane, so the test, as a user, reaches the cluster through
// those alone.
func workersCloseTheirAPI(t *testing.T, tofu outputs, layout layout) {
	for name, server := range scenarioHosts(host{address: tofu.PublicAddress.Value}, tofu) {
		connection, err := net.DialTimeout("tcp", net.JoinHostPort(server.address, apiPort), 5*time.Second)
		if err == nil {
			_ = connection.Close()
		}
		switch controlPlane := layout.hasRole(name, "control-plane"); {
		case controlPlane && err != nil:
			t.Errorf("the API of the control-plane host %s does not answer: %v", name, err)
		case !controlPlane && err == nil:
			t.Errorf("the API of the worker %s answers on its public address", name)
		}
	}
	mustClowder(t, "cluster", "status")
}

// checkCopiesOnWorkers checks that a volume's copies with a disk are on
// workers and its tiebreaker on a host that is no worker.
func checkCopiesOnWorkers(t *testing.T, layout layout, volume string) {
	t.Helper()
	for _, volumeCopy := range volumeStatus(t, volume).Copies {
		worker := layout.hasRole(volumeCopy.Host, "worker")
		switch {
		case volumeCopy.Tiebreaker && worker:
			t.Errorf("the tiebreaker of %s is on the worker %s, want the control plane", volume, volumeCopy.Host)
		case !volumeCopy.Tiebreaker && !worker:
			t.Errorf("a copy of %s with a disk is on %s, which is no worker", volume, volumeCopy.Host)
		}
	}
}

// workerTokenRefusedForControlPlane joins with a token for the workers'
// roles, as a host that claims control-plane: the cluster refuses it before
// it looks at the server.
func workerTokenRefusedForControlPlane(t *testing.T, tofu outputs) {
	token, err := jointoken.Parse(joinToken(t, 10*time.Minute, workerRoles))
	if err != nil {
		t.Fatal(err)
	}
	cluster, err := api.FindClusterByToken(context.Background(), tofu.PublicAddress.Value, api.DefaultConnectTimeout, token, buildinfo.Version)
	if err != nil {
		t.Fatal(err)
	}
	// the roles are checked before the server, so any server ID will do
	request := joinRequest(t, 1, "10.0.0.252")
	if request.Roles, err = hostrole.Parse("control-plane"); err != nil {
		t.Fatal(err)
	}
	_, err = cluster.Join(context.Background(), request)
	var statusError *api.StatusError
	want := "allows the roles " + workerRoles + ", not control-plane"
	if !errors.As(err, &statusError) || statusError.Status != http.StatusForbidden || !strings.Contains(err.Error(), want) {
		t.Errorf("want a refusal with %q, got %v", want, err)
	}
}

// workerRefusedByControlPlane calls the first host with a worker's own
// certificate, as root on the worker, or an attacker with its key, could:
// a route for every host answers, a route for the control plane refuses it
// (403), and Raft refuses it in the TLS handshake. A control-plane host's
// certificate passes Raft's handshake, so the refusal is the worker's.
func workerRefusedByControlPlane(t *testing.T, hosts map[string]host) {
	first := nodeAddressOf(t, hosts[firstHost])
	worker := hosts[thirdJoining]
	call := func(server host, method, url string) (code, output string) {
		t.Helper()
		// -k: the test checks who the server refuses, not the server's name
		command := fmt.Sprintf("curl -sS -k -X %s --cert %s --key %s -o /dev/null -w '%%{http_code}' %s 2>&1",
			method, hostCertificateFile, hostKeyFile, url)
		output, _ = server.run(command) // a refused handshake fails curl
		return output[max(0, len(output)-3):], output
	}
	firstAPI := "https://" + net.JoinHostPort(first, apiPort)
	if code, output := call(worker, http.MethodGet, firstAPI+"/v1/internal/commit-index"); code == "403" || code == "000" {
		t.Errorf("the worker's certificate on a route for every host: %s", output)
	}
	if code, output := call(worker, http.MethodPost, firstAPI+"/v1/internal/changes"); code != "403" {
		t.Errorf("the worker's certificate on a route for the control plane: %s, want 403", output)
	}
	// Raft speaks no HTTP: curl fails either way, so the difference is the
	// TLS alert that refuses the worker. For the control-plane host, curl's
	// own trace must show the handshake done, so a closed port cannot pass.
	raft := "https://" + net.JoinHostPort(first, raftPort) + "/"
	if code, output := call(worker, http.MethodGet, raft); code != "000" || !strings.Contains(output, "alert") {
		t.Errorf("Raft took the worker's certificate in the handshake: %s", output)
	}
	accepted, _ := hosts[firstJoining].run(fmt.Sprintf("curl -sS -v -k --cert %s --key %s -o /dev/null %s 2>&1",
		hostCertificateFile, hostKeyFile, raft))
	// Raft closes the connection after curl's request with the alert close
	// notify, which refuses nothing; Go refuses a certificate with the alert
	// bad certificate
	if !strings.Contains(accepted, "SSL connection using") || strings.Contains(accepted, "bad certificate") {
		t.Errorf("Raft refused a control-plane host's certificate: %s", accepted)
	}
}

// workerRefusesTheCLI calls a worker's API with the CLI's certificate from a
// control-plane host, inside the private network where the worker listens:
// the worker refuses it in the TLS handshake, while the control-plane host
// itself serves it.
func workerRefusesTheCLI(t *testing.T, hosts map[string]host, tofu outputs) {
	config, err := clientconfig.Read(credentialsFile())
	if err != nil {
		t.Fatal(err)
	}
	cliContext, err := config.Context("")
	if err != nil {
		t.Fatal(err)
	}
	controlPlane := hosts[firstJoining]
	for file, content := range map[string]string{"/tmp/live-cli.pem": cliContext.Certificate, "/tmp/live-cli-key.pem": cliContext.Key} {
		if _, err := controlPlane.runWithInput("umask 077 && cat > "+file, strings.NewReader(content)); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _, _ = controlPlane.run("rm -f /tmp/live-cli.pem /tmp/live-cli-key.pem") })
	call := func(address string) string {
		t.Helper()
		output, _ := controlPlane.run(fmt.Sprintf("curl -sS -k --cert /tmp/live-cli.pem --key /tmp/live-cli-key.pem "+
			"-o /dev/null -w '%%{http_code}' https://%s/v1/status 2>&1", net.JoinHostPort(address, apiPort)))
		return output
	}
	if output := call(tofu.Hosts.Value[thirdJoining].PrivateAddress); !strings.HasSuffix(output, "000") ||
		!strings.Contains(output, "alert") {
		t.Errorf("the worker took the CLI's certificate in the handshake: %s", output)
	}
	// the CLI's route answers; the version check may turn the bare curl
	// away, but only after the certificate was taken
	if output := call(tofu.Hosts.Value[firstJoining].PrivateAddress); strings.HasSuffix(output, "000") ||
		strings.HasSuffix(output, "403") {
		t.Errorf("the control-plane host refused the CLI's certificate: %s", output)
	}
}

// dataKeyFile is the data key, as the agent keeps it (agent.Files).
const dataKeyFile = "/var/lib/clowder/keys/data-key.txt"

// workerGetsOnlyItsSecrets runs the stack keyed, which names the secret
// keyed-token, next to the secret keyed-other, which no stack names. The
// worker that runs keyed holds no data key, and the control plane gives it
// keyed-token and not keyed-other, as Kubernetes' node authorizer gives a
// node only the secrets of its pods.
func workerGetsOnlyItsSecrets(t *testing.T, hosts map[string]host, layout layout) {
	stack := document(resource.KindStack, "keyed", "services:\n  app:\n    image: "+publicImage+
		"\n    command: [sleep, infinity]\n    stopGracePeriod: 1s\n    secrets: [keyed-token]\n"+
		"    resources: {reserve: {cpus: 0.01, memory: 16M}}\n")
	// a sealed secret is a document without a separator of its own
	createAndApply(t, "keyed", sealSecret(t, "keyed-token", "the token")+"\n---\n"+
		sealSecret(t, "keyed-other", "another")+"\n"+stack)
	t.Cleanup(func() { removeStack(t, "keyed") })
	waitRunning(t, "keyed", 1)
	on := stackInfo(t, "keyed").Host
	worker := hosts[on]
	if layout.hasRole(on, "control-plane") {
		t.Fatalf("keyed runs on %s, a control-plane host", on)
	}
	for name, server := range hosts {
		_, err := server.run("test -e " + dataKeyFile)
		switch controlPlane := layout.hasRole(name, "control-plane"); {
		case controlPlane && err != nil:
			t.Errorf("the control-plane host %s has no data key", name)
		case !controlPlane && err == nil:
			t.Errorf("the worker %s has a data key", name)
		}
	}
	// as root on the worker could ask, with the worker's own certificate
	first := nodeAddressOf(t, hosts[firstHost])
	output := worker.must(t, fmt.Sprintf("curl -sS -k --fail-with-body --cert %s --key %s https://%s/v1/internal/hosts/self/secrets",
		hostCertificateFile, hostKeyFile, net.JoinHostPort(first, apiPort)))
	var secrets map[string]api.HostSecret
	if err := json.Unmarshal([]byte(output), &secrets); err != nil {
		t.Fatalf("the worker's secrets: %v: %s", err, output)
	}
	if token, ok := secrets["default/keyed-token"]; !ok || string(token.Data[stackSecretKey]) != "the token" {
		t.Errorf("the worker did not get the secret of its stack: %v", slices.Sorted(maps.Keys(secrets)))
	}
	if _, ok := secrets["default/keyed-other"]; ok {
		t.Error("the worker got a secret that none of its stacks names")
	}
}
