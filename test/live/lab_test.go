//go:build live

package live

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/webcodingcat/clowder/internal/api"
	"github.com/webcodingcat/clowder/internal/clusterdir"
)

// labAddress is where the test and the tools of bin/dev live run in the
// lab's network, and where the hosts reach them.
const labAddress = "192.0.2.2"

// cloud calls HLab's Cloud API, the hoster's API of the test's cluster, and
// reads the answer into out (nil: none).
func cloud(t *testing.T, method, path string, body, out any) {
	t.Helper()
	var encoded []byte
	if body != nil {
		var err error
		if encoded, err = json.Marshal(body); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, method, os.Getenv("HCLOUD_ENDPOINT")+path, bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+os.Getenv("HCLOUD_TOKEN"))
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }() // read below
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode >= 300 {
		t.Fatalf("%s %s: HTTP %d: %s", method, path, response.StatusCode, answer)
	}
	if out != nil {
		if err := json.Unmarshal(answer, out); err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
	}
}

// download reads a URL of the lab (HLab's ACME CA).
func download(t *testing.T, url string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }() // read below
	content, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: HTTP %d, %v", url, response.StatusCode, err)
	}
	return content
}

// changeConfig edits the test's cluster.yaml and applies it.
func changeConfig(t *testing.T, change func(config *clusterdir.Config)) {
	t.Helper()
	file := filepath.Join(clusterDir(), clusterdir.ConfigFile)
	content, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var config clusterdir.Config
	decoder := yaml.NewDecoder(bytes.NewReader(content))
	decoder.KnownFields(true)
	if err := decoder.Decode(&config); err != nil {
		t.Fatal(err)
	}
	change(&config)
	if content, err = yaml.Marshal(config); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, content, 0o644); err != nil {
		t.Fatal(err)
	}
	mustClowder(t, "cluster", "apply")
}

// setClusterSecrets adds secrets to secrets.sops.yaml; the next cluster
// apply sends them.
func setClusterSecrets(t *testing.T, values map[string]string) {
	t.Helper()
	file := filepath.Join(clusterDir(), clusterdir.SecretsFile)
	secrets := readSecrets(t, file)
	for name, value := range values {
		secrets.Values[name] = value
	}
	if err := clusterdir.WriteSOPS(file, secrets, recipient); err != nil {
		t.Fatal(err)
	}
}

// writeClusterFile puts a file next to cluster.yaml, as acme.ca and
// smtp.ca name them.
func writeClusterFile(t *testing.T, name string, content []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(clusterDir(), name), content, 0o644); err != nil {
		t.Fatal(err)
	}
}

// createAndApply creates a stack and applies its Compose file.
func createAndApply(t *testing.T, name, content string) {
	t.Helper()
	mustClowder(t, "stack", "create", name, "--dir", stackDir(name), "--age", recipient)
	file := filepath.Join(stackDir(name), "clowder.yaml")
	if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	mustClowder(t, "stack", "apply", name, "--file", file)
}

// stackInfo is a stack as the cluster lists it.
func stackInfo(t *testing.T, name string) api.StackInfo {
	t.Helper()
	stacks, err := listStacks()
	if err != nil {
		t.Fatal(err)
	}
	return stacks[name]
}

// runs tells whether a service of a stack has a running container.
func (h host) runs(t *testing.T, stack, service string) bool {
	t.Helper()
	return h.must(t, "docker ps -q --filter label=com.docker.compose.project="+stack+
		" --filter label=com.docker.compose.service="+service) != ""
}

// clusterStatus is what clowder cluster status prints.
func clusterStatus(t *testing.T) string {
	t.Helper()
	status, err := clowder("cluster", "status")
	if err != nil {
		t.Fatal(err)
	}
	return status
}

// receiver takes the alerts of the Discord and webhook channels and the
// heartbeat over HTTP, in the lab's network, and keeps what came by path.
type receiver struct {
	mu       sync.Mutex
	requests map[string][]string
}

// receiverPort is where the receiver listens on labAddress.
const receiverPort = 8098

// startReceiver serves until the step ends.
func startReceiver(t *testing.T) *receiver {
	t.Helper()
	received := &receiver{requests: map[string][]string{}}
	listener, err := net.Listen("tcp", net.JoinHostPort(labAddress, strconv.Itoa(receiverPort)))
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			received.mu.Lock()
			received.requests[r.URL.Path] = append(received.requests[r.URL.Path], string(body))
			received.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return received
}

// url is the receiver's URL of a path.
func (r *receiver) url(path string) string {
	return fmt.Sprintf("http://%s:%d%s", labAddress, receiverPort, path)
}

// got are the bodies that came to a path.
func (r *receiver) got(path string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.requests[path]...)
}

// hasMessage tells whether a path got the message of an alert's event, as
// the webhook posts it.
func (r *receiver) hasMessage(path, event, keyPart string) bool {
	for _, body := range r.got(path) {
		var message struct{ Event, Key string }
		if json.Unmarshal([]byte(body), &message) == nil && message.Event == event && strings.Contains(message.Key, keyPart) {
			return true
		}
	}
	return false
}

// The agent's /metrics on the host's node address (host 0), where the
// host's own Prometheus scrapes it.
const (
	metricsTarget = "10.77.0.1:7444"
	metricsURL    = "http://" + metricsTarget + "/metrics"
)

// metricValue reads the first sample of a metric from the host's /metrics.
func metricValue(t *testing.T, server host, name string) float64 {
	t.Helper()
	for _, line := range strings.Split(server.must(t, "curl -sf "+metricsURL), "\n") {
		if strings.HasPrefix(line, name+"{") || strings.HasPrefix(line, name+" ") {
			fields := strings.Fields(line)
			value, err := strconv.ParseFloat(fields[len(fields)-1], 64)
			if err != nil {
				t.Fatalf("%s: %v", line, err)
			}
			return value
		}
	}
	t.Fatalf("no %s in /metrics", name)
	return 0
}
