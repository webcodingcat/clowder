package api

import (
	"context"
	"crypto/tls"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/webcodingcat/clowder/internal/state"
)

func TestTheClientTriesTheNextEndpoint(t *testing.T) {
	cluster := newTestCluster(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := listener.Addr().String()
	_ = listener.Close() // nobody listens here now
	endpoints := []string{dead, cluster.servers[0].Listener.Addr().String()}
	client := NewClient(endpoints, time.Second, cluster.cliCertificate(), cluster.ca.Certificate, testVersion)
	var status Status
	if err := client.Get(context.Background(), "/v1/status", &status); err != nil {
		t.Fatal(err)
	}
	if status.Leader != "node0" {
		t.Errorf("status %+v", status)
	}
	// a write that could not leave goes on to the next endpoint
	if _, err := client.Post(context.Background(), "/v1/hosts/0/drain", nil); err != nil {
		t.Errorf("a write after an unreachable endpoint: %v", err)
	}
}

// silentEndpoint takes connections and never answers, as a host whose network
// hangs; it counts the connections it took.
func silentEndpoint(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var taken atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		var held []net.Conn
		defer func() {
			for _, connection := range held {
				_ = connection.Close()
			}
		}()
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			taken.Add(1)
			held = append(held, connection)
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		<-done
	})
	return listener.Addr().String(), &taken
}

func TestTheClientGivesUpOnASilentEndpointAfterTheConnectTimeout(t *testing.T) {
	cluster := newTestCluster(t)
	silent, _ := silentEndpoint(t)
	endpoints := []string{silent, cluster.servers[0].Listener.Addr().String()}
	// a client each, so that each request meets the silent endpoint first
	newClient := func() *Client {
		return NewClient(endpoints, 200*time.Millisecond, cluster.cliCertificate(), cluster.ca.Certificate, testVersion)
	}
	started := time.Now()
	if err := newClient().Get(context.Background(), "/v1/status", &Status{}); err != nil {
		t.Errorf("a read after a silent endpoint: %v", err)
	}
	// a write that never got past the handshake goes on as well
	if _, err := newClient().Post(context.Background(), "/v1/hosts/0/drain", nil); err != nil {
		t.Errorf("a write after a silent endpoint: %v", err)
	}
	if err := newClient().Stream(context.Background(), "/v1/status", io.Discard); err != nil {
		t.Errorf("a stream after a silent endpoint: %v", err)
	}
	// generous for the race detector; the request's own timeout would be
	// 30 s each
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Errorf("three requests took %s", elapsed)
	}
}

func TestTheClientStartsWithTheEndpointThatAnsweredLast(t *testing.T) {
	cluster := newTestCluster(t)
	silent, taken := silentEndpoint(t)
	endpoints := []string{silent, cluster.servers[0].Listener.Addr().String()}
	client := NewClient(endpoints, 200*time.Millisecond, cluster.cliCertificate(), cluster.ca.Certificate, testVersion)
	for range 3 {
		if err := client.Get(context.Background(), "/v1/status", &Status{}); err != nil {
			t.Fatal(err)
		}
	}
	if got := taken.Load(); got != 1 {
		t.Errorf("the silent endpoint was tried %d times, want once", got)
	}
}

func TestTheClientNamesAClusterOfAnotherCA(t *testing.T) {
	cluster, other := newTestCluster(t), newTestCluster(t)
	// credentials of the other cluster, against this one's agent
	client := NewClient([]string{cluster.servers[0].Listener.Addr().String()}, time.Second,
		other.cliCertificate(), other.ca.Certificate, testVersion)
	err := client.Get(context.Background(), "/v1/status", &Status{})
	if err == nil || !strings.Contains(err.Error(), "belongs to another cluster than your context") {
		t.Errorf("read: %v", err)
	}
	if err := client.Stream(context.Background(), "/v1/watch?kind=stack&allNamespaces=true", io.Discard); err == nil ||
		!strings.Contains(err.Error(), "belongs to another cluster") {
		t.Errorf("stream: %v", err)
	}
}

func TestAWriteThatLeftIsNotSentAgain(t *testing.T) {
	cluster := newTestCluster(t)
	// this agent takes the request and hangs up without an answer
	hangsUp := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		connection, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = connection.Close()
		}
	}))
	hangsUp.TLS = &tls.Config{Certificates: []tls.Certificate{cluster.hostCertificate(1)}, MinVersion: tls.VersionTLS13}
	hangsUp.Config.ErrorLog = log.New(io.Discard, "", 0)
	hangsUp.StartTLS()
	defer hangsUp.Close()

	endpoints := []string{hangsUp.Listener.Addr().String(), cluster.servers[0].Listener.Addr().String()}
	client := NewClient(endpoints, time.Second, cluster.cliCertificate(), cluster.ca.Certificate, testVersion)
	_, err := client.Post(context.Background(), "/v1/hosts/0/drain", nil)
	if err == nil || !strings.Contains(err.Error(), "may have gone through") {
		t.Errorf("want a warning that the write may have gone through, got %v", err)
	}
	cluster.clusters[0].Read(func(current *state.State) {
		if current.Hosts[0].Phase != state.Active {
			t.Error("the write went to the second endpoint as well")
		}
	})
}
