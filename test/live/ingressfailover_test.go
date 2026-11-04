//go:build live

package live

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// failoverDomain is the echo stack's domain in TestIngressFailover: one
// without a certificate (TestIngress may leave one for its own), so the
// route answers plain HTTP.
const failoverDomain = "relay." + zone

// TestIngressFailover serves the echo stack from another host than the
// ingress host, and moves both: the client's address over HTTP and UDP,
// no packet with the Floating IP as source from another host, a container's
// way back to the stack (hairpin), a UDP flow that follows the stack, and
// the Floating IP after a drain and a failure of its host, which keeps it
// when that host is back.
func TestIngressFailover(t *testing.T) {
	server, tofu := baseline(t)
	hosts := scenarioHosts(server, tofu)
	joined := undrainJoined(t)
	cleanUpFaults(t)
	fip := tofu.FloatingIP.Value
	var stackHost string
	runSteps(t, []step{
		{"the echo stack runs on another host than the ingress host", func(t *testing.T) {
			createAndApply(t, "echo", strings.ReplaceAll(echoStack, domain, failoverDomain))
			waitRunning(t, "echo", 1)
			stackHost = joined[0]
			moveTo(t, "echo", stackHost)
			if ingress := ingressHost(t, fip); ingress != firstHost {
				t.Fatalf("the Floating IP is on %s, want %s", ingress, firstHost)
			}
			left := floatingIPLeaves(t, hosts[stackHost], fip, func() {
				httpSeesClient(t, fip)
				udpSeesClient(t, tofu)
			})
			if left != 0 {
				t.Errorf("%d packets left %s with the Floating IP as source", left, stackHost)
			}
		}},
		{"a container on a third host reaches the stack through the Floating IP", func(t *testing.T) {
			third := otherThan(joined, stackHost)
			containerReaches(t, hosts[third], fip)
		}},
		{"a UDP flow that never pauses follows the stack's move", func(t *testing.T) {
			flow := startUDPFlow(t, fip)
			flow.waitAnswer(t, time.Now())
			stackHost = otherThan(joined, stackHost)
			moveTo(t, "echo", stackHost)
			flow.waitAnswer(t, time.Now())
		}},
		{"the Floating IP follows a drain, and stays when the host takes stacks again", func(t *testing.T) {
			mustClowder(t, "host", "drain", firstHost)
			moved := waitIngressOff(t, fip, firstHost, 2*time.Minute)
			httpSeesClient(t, fip)
			udpSeesClient(t, tofu)
			mustClowder(t, "host", "undrain", firstHost)
			stays(t, time.Minute, "the Floating IP on "+moved, func() error {
				if now := ingressHost(t, fip); now != moved {
					return fmt.Errorf("it moved to %s", now)
				}
				return nil
			})
		}},
		{"the ingress host fails, and the Floating IP goes to another", func(t *testing.T) {
			gone := ingressHost(t, fip)
			if gone == stackHost {
				// a host without stacks is not fenced: this step is about the
				// Floating IP alone
				stackHost = otherThan(joined, gone)
				moveTo(t, "echo", stackHost)
			}
			start := time.Now()
			labControl(t, "/vm/"+gone+"/pause", nil)
			now := waitIngressOff(t, fip, gone, failoverBudget)
			t.Logf("the Floating IP went from %s to %s in %s", gone, now, time.Since(start).Round(time.Second))
			httpSeesClient(t, fip)
			udpSeesClient(t, tofu)
			labControl(t, "/vm/"+gone+"/cont", nil)
			waitPhase(t, gone, "active", backDeadline)
			if back := ingressHost(t, fip); back != now {
				t.Errorf("the Floating IP went back to %s", back)
			}
		}},
		{"the hosts are as the baseline wants them again", func(t *testing.T) {
			removeStack(t, "echo")
			// the first host is the only one to take stacks again, so the
			// Floating IP comes back to it
			drainJoined(t)
			eventually(t, 2*time.Minute, "the Floating IP on "+firstHost, func() error {
				if now := ingressHost(t, fip); now != firstHost {
					return fmt.Errorf("on %s", now)
				}
				return nil
			})
		}},
	})
}

// ingressHost is the host whose server holds the Floating IP; "" if none.
func ingressHost(t *testing.T, fip string) string {
	t.Helper()
	_, server := floatingIP(t, fip)
	if server == nil {
		return ""
	}
	for _, found := range clusterHosts(t) {
		if found.Phase == "removed" {
			continue
		}
		if id, _ := serverOf(t, found.Name); id == *server {
			return found.Name
		}
	}
	return ""
}

// waitIngressOff waits until the Floating IP is on another host than gone,
// and returns that host.
func waitIngressOff(t *testing.T, fip, gone string, deadline time.Duration) string {
	t.Helper()
	var now string
	eventually(t, deadline, "the Floating IP off "+gone, func() error {
		if now = ingressHost(t, fip); now == "" || now == gone {
			return fmt.Errorf("on %q", now)
		}
		return nil
	})
	return now
}

// httpSeesClient asks the echo stack's domain over plain HTTP through the
// Floating IP: it answers with the X-Forwarded-For it got, which starts with
// the test's own address. Through the ingress host's Traefik to another
// host's, the ingress host's node address follows it, as that Traefik
// adds the hop it got the request from.
func httpSeesClient(t *testing.T, fip string) {
	t.Helper()
	var dialer net.Dialer
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, net.JoinHostPort(fip, "80"))
		},
	}}
	eventually(t, time.Minute, "HTTP sees the client's address", func() error {
		body, err := get(client, "http://"+failoverDomain+"/")
		if first, _, _ := strings.Cut(body, ","); err == nil && first != labAddress {
			return fmt.Errorf("X-Forwarded-For %q, want it to start with %s", body, labAddress)
		}
		return err
	})
}

// floatingIPLeaves counts the packets that leave a host with the Floating
// IP as source while during runs, in a table of its own: only the ingress
// host may send them (the hoster routes the address to it alone).
func floatingIPLeaves(t *testing.T, server host, fip string, during func()) int {
	t.Helper()
	server.must(t, "nft add table ip fipprobe && nft add chain ip fipprobe out "+
		"'{ type filter hook postrouting priority 100; policy accept; }' && "+
		"nft add rule ip fipprobe out ip saddr "+fip+" counter")
	defer server.must(t, "nft delete table ip fipprobe")
	during()
	listed := server.must(t, "nft list table ip fipprobe")
	_, after, found := strings.Cut(listed, "counter packets ")
	if !found {
		t.Fatalf("no counter in:\n%s", listed)
	}
	packets, err := strconv.Atoi(strings.Fields(after)[0])
	if err != nil {
		t.Fatal(err)
	}
	return packets
}

// containerReaches runs a container on a host whose echo stack runs
// elsewhere: its datagram to the Floating IP reaches the stack from the
// host's node address (hairpin), and so does its HTTP request, through the
// host's own Traefik.
func containerReaches(t *testing.T, server host, fip string) {
	t.Helper()
	node := nodeAddressOf(t, server)
	udp := server.must(t, "docker run --rm "+pythonImage+` python -c 'import socket; `+
		`s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM); s.settimeout(5); `+
		`s.sendto(b"who am I", ("`+fip+`", 9987)); print(s.recvfrom(64)[0].decode())'`)
	if udp != node {
		t.Errorf("the UDP service saw the container as %q, want the node address %s", udp, node)
	}
	status := server.must(t, "docker run --rm "+pythonImage+` python -c 'import urllib.request; `+
		`request = urllib.request.Request("http://`+fip+`/", headers={"Host": "`+failoverDomain+`"}); `+
		`print(urllib.request.urlopen(request, timeout=10).status)'`)
	if status != "200" {
		t.Errorf("the container got %s from http://%s/", status, failoverDomain)
	}
}

// udpFlow sends a datagram to the Floating IP's port 9987 every 200 ms from
// one socket, so conntrack keeps one entry for it, and notes when an
// answer last came.
type udpFlow struct {
	mu       sync.Mutex
	answered time.Time
}

// startUDPFlow starts the flow until the step ends.
func startUDPFlow(t *testing.T, fip string) *udpFlow {
	t.Helper()
	conn, err := net.Dial("udp", net.JoinHostPort(fip, "9987"))
	if err != nil {
		t.Fatal(err)
	}
	flow := &udpFlow{}
	done := make(chan struct{})
	t.Cleanup(func() {
		close(done)
		_ = conn.Close()
	})
	go func() {
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				_, _ = conn.Write([]byte("still here")) // a lost one is the point
			}
		}
	}()
	go func() {
		answer := make([]byte, 64)
		for {
			if _, err := conn.Read(answer); err != nil {
				select {
				case <-done:
					return
				default:
					continue // ICMP from a host that has no listener yet
				}
			}
			flow.mu.Lock()
			flow.answered = time.Now()
			flow.mu.Unlock()
		}
	}()
	return flow
}

// waitAnswer waits until an answer came after since.
func (f *udpFlow) waitAnswer(t *testing.T, since time.Time) {
	t.Helper()
	eventually(t, time.Minute, "an answer to the UDP flow", func() error {
		f.mu.Lock()
		defer f.mu.Unlock()
		if !f.answered.After(since) {
			return fmt.Errorf("the last at %s", f.answered.Format(time.TimeOnly))
		}
		return nil
	})
}
