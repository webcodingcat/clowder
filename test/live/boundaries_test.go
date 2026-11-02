//go:build live

package live

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/webcodingcat/clowder/internal/resource"
)

// The agent's ports (agent.APIPort, agent.RaftPort, agent.MetricsPort);
// copied, as the host's files are: the test checks the host as a container
// would find it.
const (
	apiPort     = "7443"
	raftPort    = "7400"
	metricsPort = "7444"
)

// probeSeconds is how long a probe waits for a connection: a dropped packet
// gets no answer, so a blocked probe always takes this long.
const probeSeconds = 3

// neighborStack listens on a port of its own network, which another
// stack's container must not reach.
var neighborStack = document(resource.KindStack, "neighbor", `services:
  app:
    image: `+pythonImage+`
    command: [python, -m, http.server, "8080"]
    stopGracePeriod: 1s
    resources: {reserve: {cpus: 0.01, memory: 32M}}
`)

// TestBoundaries checks what a container may reach on one host: the rules
// of internal/network (guard_in, guard_fwd) and Docker's own rules between
// stacks, which a Docker update could change.
func TestBoundaries(t *testing.T) {
	server, tofu := baseline(t)
	runSteps(t, []step{
		{"a container reaches neither the host nor another stack", func(t *testing.T) {
			checkBoundaries(t, server, tofu)
		}},
		{"the stacks are deleted", func(t *testing.T) {
			removeStack(t, "prober")
			removeStack(t, "neighbor")
		}},
	})
}

// checkBoundaries probes from a container of the stack prober. Each target
// must answer the host itself first, so a blocked probe means the rules and
// not a target that is down; the registry beyond the host and the resolver
// must answer the container, so a probe that cannot get out shows.
func checkBoundaries(t *testing.T, server host, tofu outputs) {
	createAndApply(t, "prober", simpleStack("prober", ""))
	createAndApply(t, "neighbor", neighborStack)
	waitRunning(t, "prober", 1)
	waitRunning(t, "neighbor", 1)
	prober := server.container(t, "prober", "app")
	proberPID := server.must(t, "docker inspect --format '{{.State.Pid}}' "+prober)
	// the stack's one network
	neighborIPs := strings.Fields(server.must(t, "docker inspect --format "+
		"'{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}' "+server.container(t, "neighbor", "app")))
	if len(neighborIPs) != 1 {
		t.Fatalf("neighbor's container has the addresses %v, want one", neighborIPs)
	}
	// clowder0 holds the resolver's address too; the node address is the one
	// in the overlay (network.Overlay)
	node := server.must(t, "ip -4 -o addr show dev clowder0 to 10.77.0.0/24 | awk '{print $4}' | cut -d / -f 1")
	if net.ParseIP(node) == nil {
		t.Fatalf("the node address on clowder0: %q", node)
	}

	for _, target := range []struct{ what, address string }{
		{"the metadata service", "169.254.169.254:80"},
		{"the API on the node address", net.JoinHostPort(node, apiPort)},
		{"Raft", net.JoinHostPort(node, raftPort)},
		{"/metrics", net.JoinHostPort(node, metricsPort)},
		{"the API on the underlay", net.JoinHostPort(tofu.PrivateAddress.Value, apiPort)},
		{"SSH on the underlay", net.JoinHostPort(tofu.PrivateAddress.Value, "22")},
		{"the API on the public address", net.JoinHostPort(tofu.PublicAddress.Value, apiPort)},
		{"SSH on the public address", net.JoinHostPort(tofu.PublicAddress.Value, "22")},
		{"SSH on the Floating IP", net.JoinHostPort(tofu.FloatingIP.Value, "22")},
		{"another stack's container", net.JoinHostPort(neighborIPs[0], "8080")},
	} {
		if !reaches(t, server, "", target.address) {
			t.Errorf("%s (%s) does not answer the host itself; the probe would tell nothing", target.what, target.address)
			continue
		}
		if reaches(t, server, proberPID, target.address) {
			t.Errorf("a container reaches %s (%s)", target.what, target.address)
		}
	}

	if !reaches(t, server, proberPID, registry) {
		t.Errorf("a container does not reach the registry beyond the host (%s): the probe cannot get out", registry)
	}
	directory, err := url.Parse(os.Getenv("HLAB_ACME_DIRECTORY"))
	if err != nil {
		t.Fatal(err)
	}
	// DNS from inside the container, with its own resolv.conf: a TCP probe
	// would not show that the resolver answers
	if output, err := server.run("docker exec " + prober + " nslookup " + directory.Hostname()); err != nil {
		t.Errorf("the resolver does not answer a container: %v\n%s", err, output)
	}
}

// reaches tells whether a TCP connection to address opens from the network
// namespace of the process pid, or from the host's own with pid "". It runs
// the probe on the host with its bash, as a container's image may have no
// tool for it; the packets still leave through the container's bridge.
func reaches(t *testing.T, server host, pid, address string) bool {
	t.Helper()
	ip, port, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	probe := fmt.Sprintf("timeout %d bash -c '</dev/tcp/%s/%s'", probeSeconds, ip, port)
	if pid != "" {
		probe = "nsenter --target " + pid + " --net " + probe
	}
	switch answer := server.must(t, probe+" 2>/dev/null && echo open || echo closed"); answer {
	case "open":
		return true
	case "closed":
		return false
	default:
		t.Fatalf("probe of %s: %q", address, answer)
		return false
	}
}
