package notify

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/webcodingcat/clowder/internal/clusterdir"
)

var testMessage = Message{
	Cluster: "prod", Event: Raised, Key: "pool-use/node0/vg/thin/data", Kind: "pool-use",
	Severity: clusterdir.SeverityCritical, Subject: "pool vg/thin on node0 is full", Text: "95 % of its data is used.",
}

// secrets opens every secret as its own name with a prefix.
func secrets(name string) (string, error) { return "opened-" + name, nil }

func TestTheWebhookPostsTheMessageAsJSON(t *testing.T) {
	var got Message
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	notifier := webhook{url: server.URL}
	if err := notifier.Send(context.Background(), testMessage); err != nil {
		t.Fatal(err)
	}
	if got != testMessage {
		t.Errorf("posted %+v", got)
	}
}

func TestDiscordGetsAnEmbedColoredBySeverity(t *testing.T) {
	var got struct {
		Embeds []struct {
			Title string
			Color int
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
	}))
	defer server.Close()
	notifier, err := New(clusterdir.AlertChannel{Type: "discord", Webhook: &clusterdir.SecretRef{Secret: "hook"}},
		func(string) (string, error) { return server.URL, nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := notifier.Send(context.Background(), testMessage); err != nil {
		t.Fatal(err)
	}
	if len(got.Embeds) != 1 || got.Embeds[0].Color != 0xD32F2F ||
		got.Embeds[0].Title != "[prod] critical: pool vg/thin on node0 is full" {
		t.Errorf("posted %+v", got)
	}
	resolved := testMessage
	resolved.Event = Resolved
	if err := notifier.Send(context.Background(), resolved); err != nil {
		t.Fatal(err)
	}
	if got.Embeds[0].Color != 0x388E3C || !strings.HasPrefix(got.Embeds[0].Title, "[prod] resolved:") {
		t.Errorf("the all-clear %+v", got)
	}
}

func TestAFailedPostSaysWhyWithoutTheSecretURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unknown webhook", http.StatusNotFound)
	}))
	defer server.Close()
	err := webhook{url: server.URL + "/secret-token"}.Send(context.Background(), testMessage)
	if err == nil || !strings.Contains(err.Error(), "HTTP 404") || strings.Contains(err.Error(), "secret-token") {
		t.Errorf("got %v", err)
	}
	err = webhook{url: "http://127.0.0.1:1/secret-token"}.Send(context.Background(), testMessage)
	if err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Errorf("an unreachable channel: %v", err)
	}
}

func TestTheHeartbeatIsAGet(t *testing.T) {
	var method string
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { method = r.Method }))
	defer server.Close()
	if err := Heartbeat(context.Background(), server.URL); err != nil || method != http.MethodGet {
		t.Errorf("%s, %v", method, err)
	}
}

// testCertificate is a self-signed certificate for 127.0.0.1, and its PEM.
func testCertificate(t *testing.T) (tls.Certificate, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key},
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// fakeSMTP is a mail server that offers STARTTLS (unless plain) and keeps
// what it got.
type fakeSMTP struct {
	certificate tls.Certificate
	plain       bool

	mu   sync.Mutex
	auth string
	mail string
}

// serve answers one SMTP session on conn.
func (f *fakeSMTP) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	reader, writer := bufio.NewReader(conn), conn.(io.Writer)
	say := func(line string) { _, _ = io.WriteString(writer, line+"\r\n") }
	say("220 fake ESMTP")
	inData := false
	var data strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if inData {
			if line == "." {
				inData = false
				f.mu.Lock()
				f.mail = data.String()
				f.mu.Unlock()
				say("250 queued")
				continue
			}
			data.WriteString(line + "\n")
			continue
		}
		command := strings.ToUpper(strings.Fields(line + " x")[0])
		switch command {
		case "EHLO":
			_, isTLS := conn.(*tls.Conn)
			switch {
			case !isTLS && !f.plain:
				say("250-fake")
				say("250 STARTTLS")
			default:
				say("250-fake")
				say("250 AUTH PLAIN")
			}
		case "STARTTLS":
			say("220 go ahead")
			tlsConn := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{f.certificate}, MinVersion: tls.VersionTLS12})
			if tlsConn.Handshake() != nil {
				return
			}
			conn, reader, writer = tlsConn, bufio.NewReader(tlsConn), tlsConn
		case "AUTH":
			f.mu.Lock()
			f.auth = line
			f.mu.Unlock()
			say("235 ok")
		case "DATA":
			inData = true
			say("354 go on")
		case "QUIT":
			say("221 bye")
			return
		default: // MAIL, RCPT
			say("250 ok")
		}
	}
}

// startSMTP listens on 127.0.0.1 and returns the port.
func startSMTP(t *testing.T, server *fakeSMTP) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go server.serve(conn)
		}
	}()
	return listener.Addr().(*net.TCPAddr).Port
}

func emailChannel(port int) clusterdir.AlertChannel {
	return clusterdir.AlertChannel{
		Type: "email", From: "clowder@example.com", To: []string{"ops@example.com"},
		SMTP: &clusterdir.SMTP{
			Host: "127.0.0.1", Port: port, User: "clowder", Password: &clusterdir.SecretRef{Secret: "smtp"},
			CA: "certs/ca.pem",
		},
	}
}

func TestEmailGoesOnlyThroughTLS(t *testing.T) {
	certificate, caPEM := testCertificate(t)
	files := map[string][]byte{"certs/ca.pem": caPEM}

	server := &fakeSMTP{certificate: certificate}
	notifier, err := New(emailChannel(startSMTP(t, server)), secrets, files)
	if err != nil {
		t.Fatal(err)
	}
	if err := notifier.Send(context.Background(), testMessage); err != nil {
		t.Fatal(err)
	}
	server.mu.Lock()
	auth, mail := server.auth, server.mail
	server.mu.Unlock()
	if !strings.HasPrefix(auth, "AUTH PLAIN") {
		t.Errorf("auth %q", auth)
	}
	for _, want := range []string{
		"Subject: [prod] critical: pool vg/thin on node0 is full", "To: ops@example.com", "95 % of its data",
		"MIME-Version: 1.0", "Message-ID: <",
	} {
		if !strings.Contains(mail, want) {
			t.Errorf("no %q in the mail:\n%s", want, mail)
		}
	}

	plain := &fakeSMTP{certificate: certificate, plain: true}
	notifier, err = New(emailChannel(startSMTP(t, plain)), secrets, files)
	if err != nil {
		t.Fatal(err)
	}
	if err := notifier.Send(context.Background(), testMessage); err == nil || !strings.Contains(err.Error(), "no STARTTLS") {
		t.Errorf("a server without STARTTLS: %v", err)
	}
}
