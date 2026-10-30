//go:build live

package live

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/webcodingcat/clowder/internal/clusterdir"
)

// The test's domain in a DNS zone of HLab's Cloud API, which serves it in
// the lab; HLab takes zones only right below a public suffix.
const (
	zone   = "example.com"
	domain = "shop." + zone
)

// The image of the echo stack; the host pulls it from Docker Hub.
const pythonImage = "python:3.13-alpine3.22"

// echoStack is the stack behind the domain: web answers with the
// X-Forwarded-For it got, udp with the address it got a datagram from.
const echoStack = `name: echo
services:
  web:
    image: ` + pythonImage + `
    stop_grace_period: 1s
    command:
      - python
      - -c
      - |
        import http.server
        class Echo(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                body = (self.headers.get("X-Forwarded-For") or "").encode()
                self.send_response(200)
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)
        http.server.HTTPServer(("", 8080), Echo).serve_forever()
    x-clowder:
      http: {host: ` + domain + `, port: 8080}
  udp:
    image: ` + pythonImage + `
    stop_grace_period: 1s
    ports: ["9987:9987/udp"]
    command:
      - python
      - -c
      - |
        import socket
        server = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        server.bind(("", 9987))
        while True:
            data, peer = server.recvfrom(512)
            server.sendto(peer[0].encode(), peer)
`

// certificateDeadline is how long the first certificate may take: the DNS
// check, the self-check through Traefik, the CA's validation.
const certificateDeadline = 5 * time.Minute

// acmeCAFile is HLab's ACME CA bundle next to cluster.yaml (acme.ca): the
// lab's CA for Pebble's HTTPS, and Pebble's roots, which sign the
// certificates.
const acmeCAFile = "pebble.pem"

// holdFloatingIP moves the Floating IP away by hand, through the Cloud API:
// the leader routes it back to the ingress host.
func holdFloatingIP(t *testing.T, tofu outputs) {
	id, server := floatingIP(t, tofu.FloatingIP.Value)
	if server == nil {
		t.Fatal("the Floating IP is on no server")
	}
	cloud(t, http.MethodPost, fmt.Sprintf("/floating_ips/%d/actions/unassign", id), map[string]any{}, nil)
	start := time.Now()
	eventually(t, 2*time.Minute, "the Floating IP is back on the host", func() error {
		if _, now := floatingIP(t, tofu.FloatingIP.Value); now == nil || *now != *server {
			return fmt.Errorf("on server %v", now)
		}
		return nil
	})
	t.Logf("the Floating IP was back after %s", time.Since(start).Round(time.Second))
}

// floatingIP is the ID of the Floating IP with an address, and the server
// it is on (nil: none).
func floatingIP(t *testing.T, address string) (int64, *int64) {
	t.Helper()
	var answer struct {
		FloatingIPs []struct {
			ID     int64  `json:"id"`
			IP     string `json:"ip"`
			Server *int64 `json:"server"`
		} `json:"floating_ips"`
	}
	cloud(t, http.MethodGet, "/floating_ips", nil, &answer)
	for _, ip := range answer.FloatingIPs {
		if ip.IP == address {
			return ip.ID, ip.Server
		}
	}
	t.Fatalf("no Floating IP %s", address)
	return 0, nil
}

// serveHTTPS points the domain at the Floating IP, lets the cluster order
// its certificates at HLab's Pebble and runs the echo stack: HTTPS answers
// with a certificate the CA signed and the test's own address in
// X-Forwarded-For, and HTTP redirects to it.
func serveHTTPS(t *testing.T, tofu outputs) {
	// the lab keeps a zone across runs (OpenTofu did not make it): one an
	// earlier run left points at its Floating IP
	var zones struct {
		Zones []struct {
			ID int64 `json:"id"`
		} `json:"zones"`
	}
	cloud(t, http.MethodGet, "/zones?name="+zone, nil, &zones)
	for _, left := range zones.Zones {
		cloud(t, http.MethodDelete, fmt.Sprintf("/zones/%d", left.ID), nil, nil)
	}
	cloud(t, http.MethodPost, "/zones", map[string]any{
		"name": zone, "mode": "primary",
		"rrsets": []map[string]any{{
			"name": "shop", "type": "A", "records": []map[string]string{{"value": tofu.FloatingIP.Value}},
		}},
	}, nil)
	writeClusterFile(t, acmeCAFile, download(t, os.Getenv("HLAB_ACME_CA")))
	changeConfig(t, func(config *clusterdir.Config) {
		config.ACME = clusterdir.ACME{Directory: os.Getenv("HLAB_ACME_DIRECTORY"), CA: acmeCAFile, Email: "ops@" + zone}
	})
	createAndApply(t, "echo", echoStack)
	waitRunning(t, "echo", 1)

	client := ingressClient(t, tofu.FloatingIP.Value)
	start := time.Now()
	eventually(t, certificateDeadline, "HTTPS answers with the CA's certificate", func() error {
		body, err := get(client, "https://"+domain+"/")
		if err == nil && body != labAddress {
			return fmt.Errorf("X-Forwarded-For %q, want %s", body, labAddress)
		}
		return err
	})
	t.Logf("HTTPS answered %s after the stack ran", time.Since(start).Round(time.Second))
	response, err := client.Get("http://" + domain + "/")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if location := response.Header.Get("Location"); response.StatusCode/100 != 3 || !strings.HasPrefix(location, "https://"+domain) {
		t.Errorf("HTTP answered %s with Location %q, want a redirect to HTTPS", response.Status, location)
	}
}

// ingressClient asks the Floating IP whatever the URL's host, and trusts
// HLab's ACME CA.
func ingressClient(t *testing.T, floatingIP string) *http.Client {
	t.Helper()
	pool := x509.NewCertPool()
	content, err := os.ReadFile(filepath.Join(clusterDir(), acmeCAFile))
	if err != nil || !pool.AppendCertsFromPEM(content) {
		t.Fatalf("read the ACME CA: %v", err)
	}
	var dialer net.Dialer
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			_, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(floatingIP, port))
		},
		TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
	}
	return &http.Client{
		Transport: transport, Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// get reads a URL and wants 200.
func get(client *http.Client, url string) (string, error) {
	response, err := client.Get(url) // the client has a timeout
	if err != nil {
		return "", err
	}
	defer func() { _ = response.Body.Close() }() // read below
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return "", err
	}
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: %s", response.Status, body)
	}
	return string(body), nil
}

// peerCertificate is the certificate Traefik shows for the domain.
func peerCertificate(t *testing.T, floatingIP string) *x509.Certificate {
	t.Helper()
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 10 * time.Second}, "tcp", net.JoinHostPort(floatingIP, "443"),
		// no check: the test only reads the certificate's dates and serial
		&tls.Config{ServerName: domain, InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }() // only the handshake counts
	return conn.ConnectionState().PeerCertificates[0]
}

// udpSeesClient sends a datagram to the published port: the container
// answers with the address it came from, the test's own.
func udpSeesClient(t *testing.T, tofu outputs) {
	eventually(t, time.Minute, "the UDP service answers with the client's address", func() error {
		conn, err := net.Dial("udp", net.JoinHostPort(tofu.FloatingIP.Value, "9987"))
		if err != nil {
			return err
		}
		defer func() { _ = conn.Close() }() // one datagram each way
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		if _, err := conn.Write([]byte("who am I")); err != nil {
			return err
		}
		answer := make([]byte, 64)
		count, err := conn.Read(answer)
		if err != nil {
			return err
		}
		if got := string(answer[:count]); got != labAddress {
			return fmt.Errorf("the container saw %s, want %s", got, labAddress)
		}
		return nil
	})
}

// hairpin asks the domain from inside a container: the request goes to the
// Floating IP on its own host and through Traefik back to the stack.
func hairpin(t *testing.T, server host) {
	server.upload(t, filepath.Join(clusterDir(), acmeCAFile), "/tmp/"+acmeCAFile)
	web := server.must(t, "docker ps -q --filter label=com.docker.compose.project=echo"+
		" --filter label=com.docker.compose.service=web")
	server.must(t, "docker cp /tmp/"+acmeCAFile+" "+web+":/tmp/"+acmeCAFile)
	status := server.must(t, "docker exec "+web+` python -c 'import ssl, urllib.request; `+
		`context = ssl.create_default_context(cafile="/tmp/`+acmeCAFile+`"); `+
		`print(urllib.request.urlopen("https://`+domain+`/", context=context, timeout=10).status)'`)
	if status != "200" {
		t.Errorf("the container got %s from https://%s", status, domain)
	}
}

// renewCertificate sets acme.renew_before between the certificate's time
// left and its lifetime: one new certificate comes, and none after it,
// whose time left is above the value again. Then renew_before above the
// lifetime: no renewal, the alert renew-before-ignored instead, gone with
// the value.
func renewCertificate(t *testing.T, tofu outputs) {
	first := peerCertificate(t, tofu.FloatingIP.Value)
	lifetime := first.NotAfter.Sub(first.NotBefore)
	// renew_before = lifetime less half the certificate's age: more than
	// its time left (lifetime less its age), so it is due; less than a new
	// certificate's time left as long as that one is younger than half the
	// first one's age. Four minutes of age give the test two to look.
	if age := time.Since(first.NotBefore); age < 4*time.Minute {
		time.Sleep(4*time.Minute - age)
	}
	renewBefore := lifetime - time.Since(first.NotBefore)/2
	t.Logf("certificate %s: lifetime %s, renew_before %s", first.SerialNumber, lifetime, renewBefore.Round(time.Minute))
	changeConfig(t, func(config *clusterdir.Config) {
		config.ACME.RenewBefore = fmt.Sprintf("%dm", renewBefore/time.Minute)
	})
	var second *x509.Certificate
	eventually(t, certificateDeadline, "a renewed certificate", func() error {
		second = peerCertificate(t, tofu.FloatingIP.Value)
		if second.SerialNumber.Cmp(first.SerialNumber) == 0 {
			return fmt.Errorf("still %s", first.SerialNumber)
		}
		return nil
	})
	// three passes of the leader's ACME loop (every 30 s): time to renew
	// again, if it wrongly would
	time.Sleep(90 * time.Second)
	if now := peerCertificate(t, tofu.FloatingIP.Value); now.SerialNumber.Cmp(second.SerialNumber) != 0 {
		t.Errorf("renewed again: %s after %s", now.SerialNumber, second.SerialNumber)
	}

	changeConfig(t, func(config *clusterdir.Config) {
		config.ACME.RenewBefore = fmt.Sprintf("%dm", (lifetime+24*time.Hour)/time.Minute)
	})
	eventually(t, 2*time.Minute, "the alert renew-before-ignored", func() error {
		if !strings.Contains(clusterStatus(t), "renew-before-ignored") {
			return fmt.Errorf("not raised")
		}
		return nil
	})
	changeConfig(t, func(config *clusterdir.Config) { config.ACME.RenewBefore = "" })
	eventually(t, 2*time.Minute, "renew-before-ignored resolved", func() error {
		if strings.Contains(clusterStatus(t), "renew-before-ignored") {
			return fmt.Errorf("still open")
		}
		return nil
	})
	if now := peerCertificate(t, tofu.FloatingIP.Value); now.SerialNumber.Cmp(second.SerialNumber) != 0 {
		t.Errorf("renewed with renew_before above the lifetime: %s", now.SerialNumber)
	}
}
