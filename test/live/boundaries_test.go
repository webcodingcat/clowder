//go:build live

package live

import (
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"strconv"
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

// TestBoundaries checks what a container may reach on its host and of the
// other hosts: the rules of internal/network (guard_in, guard_fwd) and
// Docker's own rules between stacks, which a Docker update could change.
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
	node := nodeAddressOf(t, server)
	targets := []struct{ what, address string }{
		{"the API on the node address", net.JoinHostPort(node, apiPort)},
		{"Raft", net.JoinHostPort(node, raftPort)},
		{"/metrics", net.JoinHostPort(node, metricsPort)},
		{"the API on the underlay", net.JoinHostPort(tofu.PrivateAddress.Value, apiPort)},
		{"SSH on the underlay", net.JoinHostPort(tofu.PrivateAddress.Value, "22")},
		{"the API on the public address", net.JoinHostPort(tofu.PublicAddress.Value, apiPort)},
		{"SSH on the public address", net.JoinHostPort(tofu.PublicAddress.Value, "22")},
		{"SSH on the Floating IP", net.JoinHostPort(tofu.FloatingIP.Value, "22")},
		{"another stack's container", net.JoinHostPort(neighborIPs[0], "8080")},
	}
	// the other hosts: the first host reaches them over the private network
	// and WireGuard, its containers must not. Their public addresses are
	// not probed: the Cloud firewall closes SSH and the API there to the
	// first host itself, so no answer could vouch for the probe; the drop
	// of @cluster_hosts names them all the same.
	for name, other := range tofu.Hosts.Value {
		if other.PublicAddress == tofu.PublicAddress.Value {
			continue
		}
		otherNode := nodeAddressOf(t, host{address: other.PublicAddress})
		targets = append(targets, []struct{ what, address string }{
			{name + "'s API on its node address", net.JoinHostPort(otherNode, apiPort)},
			{name + "'s Raft", net.JoinHostPort(otherNode, raftPort)},
			{name + "'s API on its underlay", net.JoinHostPort(other.PrivateAddress, apiPort)},
			{name + "'s SSH on its underlay", net.JoinHostPort(other.PrivateAddress, "22")},
		}...)
	}

	for _, target := range targets {
		if !reaches(t, server, "", target.address) {
			t.Errorf("%s (%s) does not answer the host itself; the probe would tell nothing", target.what, target.address)
			continue
		}
		if reaches(t, server, proberPID, target.address) {
			t.Errorf("a container reaches %s (%s)", target.what, target.address)
		}
	}

	// the HTTP hairpin: the local Traefik on the Floating IP answers a container
	if hairpin := net.JoinHostPort(tofu.FloatingIP.Value, "443"); !reaches(t, server, proberPID, hairpin) {
		t.Errorf("a container does not reach Traefik on the Floating IP (%s)", hairpin)
	}
	if !reaches(t, server, proberPID, registry) {
		t.Errorf("a container does not reach the registry beyond the host (%s): the probe cannot get out", registry)
	}
	checkDroppedBeyondTheHost(t, server, tofu, proberPID)
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

// nodeAddressOf reads a host's node address: clowder0 holds the resolver's
// address too, the node address is the one in the overlay (network.Overlay).
func nodeAddressOf(t *testing.T, server host) string {
	t.Helper()
	node := server.must(t, "ip -4 -o addr show dev clowder0 to 10.77.0.0/24 | awk '{print $4}' | cut -d / -f 1")
	if net.ParseIP(node) == nil {
		t.Fatalf("the node address on clowder0 of %s: %q", server.address, node)
	}
	return node
}

// checkDroppedBeyondTheHost probes what no answer of the host itself can
// vouch for: the metadata service, which the host's own processes must not
// reach either, an address of the private network that is no host, and a
// packet with a source outside the container's network, and a datagram to
// another host's underlay. Each must be
// dropped by its own rule of internal/network, whose counter shows it.
func checkDroppedBeyondTheHost(t *testing.T, server host, tofu outputs, proberPID string) {
	const metadata = "169.254.169.254:80"
	dropsProbe(t, server, "guard_out", "ip daddr 169.254.169.254", func() {
		if reaches(t, server, "", metadata) {
			t.Errorf("the host's own processes reach the metadata service (%s)", metadata)
		}
	})
	dropsProbe(t, server, "guard_fwd", "ip daddr 169.254.169.254", func() {
		if reaches(t, server, proberPID, metadata) {
			t.Errorf("a container reaches the metadata service (%s)", metadata)
		}
	})

	privateNetwork, err := netip.ParsePrefix(tofu.moduleConfig(t).PrivateNetwork)
	if err != nil {
		t.Fatalf("the module's privateNetwork: %v", err)
	}
	// the address before the network's last (its broadcast): no host has it
	first := privateNetwork.Addr().As4()
	size := uint32(1) << (32 - privateNetwork.Bits())
	var beforeLast [4]byte
	binary.BigEndian.PutUint32(beforeLast[:], binary.BigEndian.Uint32(first[:])+size-2)
	stranger := net.JoinHostPort(netip.AddrFrom4(beforeLast).String(), "22")
	dropsProbe(t, server, "guard_fwd", "ip daddr "+privateNetwork.String(), func() {
		if reaches(t, server, proberPID, stranger) {
			t.Errorf("a container reaches %s in the private network", stranger)
		}
	})

	inContainer := "nsenter --target " + proberPID + " --net"
	// UDP, which gets no answer to vouch for it: a datagram to another
	// host's WireGuard port on its underlay
	for _, other := range tofu.Hosts.Value {
		if other.PrivateAddress == tofu.PrivateAddress.Value {
			continue
		}
		dropsProbe(t, server, "guard_fwd", "ip daddr @cluster_hosts", func() {
			server.must(t, inContainer+" bash -c 'echo probe > /dev/udp/"+other.PrivateAddress+"/51800'; true")
		})
		break
	}

	// a source of TEST-NET-1 on the container's own interface, as a
	// container with NET_RAW could send; the registry answers it unspoofed
	const spoofed = "192.0.2.7"
	server.must(t, inContainer+" ip address add "+spoofed+"/32 dev eth0")
	defer server.must(t, inContainer+" ip address delete "+spoofed+"/32 dev eth0")
	dropsProbe(t, server, "guard_fwd", "fib saddr . iif oif missing", func() {
		server.must(t, fmt.Sprintf("%s timeout %d curl -s --interface %s http://%s/ >/dev/null; true",
			inContainer, probeSeconds, spoofed, registry))
	})
}

// dropsProbe fails the test unless probe makes the counter of a drop rule
// of the table inet clowder grow; rule is the start of the rule's match. The agent
// loads the table anew every loop interval, which zeroes the counters, so a
// few tries tell a reload from a probe that went through.
func dropsProbe(t *testing.T, server host, chain, rule string, probe func()) {
	t.Helper()
	count := func() int {
		listed := server.must(t, "nft list chain inet clowder "+chain)
		for line := range strings.Lines(listed) {
			if _, after, found := strings.Cut(line, rule+" counter packets "); found {
				if packets, err := strconv.Atoi(strings.Fields(after)[0]); err == nil {
					return packets
				}
			}
		}
		t.Fatalf("no rule %q with a counter in %s:\n%s", rule, chain, listed)
		return 0
	}
	for range 3 {
		before := count()
		probe()
		if count() > before {
			return
		}
	}
	t.Errorf("the rule %q of %s dropped nothing of the probe", rule, chain)
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
