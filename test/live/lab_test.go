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

	"github.com/webcodingcat/clowder/internal/clusterspec"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/resource"
)

// labAddress is where the test and the tools of bin/dev live run in the
// lab's network, and where the hosts reach them.
const labAddress = "192.0.2.2"

// cloud calls HLab's Cloud API, Hetzner's API of the test's cluster, and
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

// checkNoFaults fails the test while HLab injects a fault (hlab fault):
// scenarios make their faults through HLab's lab control (HLAB_CONTROL), and
// one that failed may have left its fault behind.
func checkNoFaults(t *testing.T) {
	t.Helper()
	var status struct {
		Faults   []json.RawMessage          `json:"faults"`
		Netem    map[string]json.RawMessage `json:"netem"`
		APIFault struct {
			Count   int `json:"count"`
			DelayMS int `json:"delay_ms"`
		} `json:"api_fault"`
		Servers []struct {
			Name     string `json:"name"`
			PublicIP string `json:"public_ip"`
			Dead     bool   `json:"dead"`
		} `json:"servers"`
	}
	if err := json.Unmarshal(download(t, os.Getenv("HLAB_CONTROL")+"/status"), &status); err != nil {
		t.Fatalf("read the lab's status: %v", err)
	}
	if len(status.Faults) > 0 || len(status.Netem) > 0 || status.APIFault.Count > 0 || status.APIFault.DelayMS > 0 {
		t.Fatalf("the lab injects faults (hlab status shows them, hlab fault clear ends them): %d faults, "+
			"netem on %d, API fault %+v", len(status.Faults), len(status.Netem), status.APIFault)
	}
	// a renamed field reads as zero and would pass unseen: the test's own
	// host must be listed
	listed := false
	for _, server := range status.Servers {
		if server.Dead {
			t.Fatalf("the lab's server %s is dead (hlab fault alive %s)", server.Name, server.Name)
		}
		listed = listed || server.PublicIP == readOutputs(t).PublicAddress.Value
	}
	if !listed {
		t.Fatal("the lab's status does not list the test's host; has its format changed?")
	}
}

// download reads a URL of the lab (HLab's ACME CA, its status).
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

// The configuration's directory holds the cluster's own resources, as a
// user keeps them in a repository: the Secrets of clowder-system, the
// ClusterConfiguration that names them, and the namespace default with its
// registry login. An apply takes a directory's files by name, so the
// numbers in front put each Secret of clowder-system above the
// configuration that names it.
const (
	configurationFile    = "2-configuration.yaml"
	defaultNamespaceFile = "3-default.yaml"
)

// configDir is the configuration's directory.
func configDir() string { return filepath.Join(workDir, "configuration") }

// systemSecretFile is the file of a Secret of clowder-system in the
// configuration's directory.
func systemSecretFile(name string) string {
	return filepath.Join(configDir(), "1-secret-"+name+".yaml")
}

// applyConfig applies the configuration's directory. It holds the Secret
// of the Hetzner token, so the CLI would ask first: --yes.
func applyConfig(t *testing.T) {
	t.Helper()
	mustClowder(t, "apply", "-f", configDir(), "--yes")
}

// changeConfig edits the test's ClusterConfiguration and applies it.
func changeConfig(t *testing.T, change func(config *clusterspec.Config)) {
	t.Helper()
	config := readConfig(t)
	change(&config)
	writeConfig(t, config)
	applyConfig(t)
}

// readConfig reads the spec of the test's ClusterConfiguration, strictly,
// as the cluster reads it.
func readConfig(t *testing.T) clusterspec.Config {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(configDir(), configurationFile))
	if err != nil {
		t.Fatal(err)
	}
	objects, err := resource.ParseDocuments(content)
	if err != nil || len(objects) != 1 || objects[0].Kind != resource.KindClusterConfiguration {
		t.Fatalf("read %s: %d resources, %v", configurationFile, len(objects), err)
	}
	config, err := resource.DecodeClusterConfiguration(objects[0].Spec)
	if err != nil {
		t.Fatalf("read %s: %v", configurationFile, err)
	}
	return config
}

// clusterConfigJSON is the spec of the test's ClusterConfiguration as JSON;
// it names its Secrets, so it stays the same when their values are
// encrypted anew.
func clusterConfigJSON(t *testing.T) []byte {
	t.Helper()
	content, err := json.MarshalIndent(readConfig(t), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return content
}

// writeConfig writes the test's ClusterConfiguration with config as its
// spec. The spec has JSON's names only, so it goes through JSON into YAML,
// as a user would write it.
func writeConfig(t *testing.T, config clusterspec.Config) {
	t.Helper()
	encoded, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	var tree any
	if err := json.Unmarshal(encoded, &tree); err != nil {
		t.Fatal(err)
	}
	spec, err := yaml.Marshal(tree)
	if err != nil {
		t.Fatal(err)
	}
	content := documentIn("", resource.KindClusterConfiguration, resource.ClusterConfigurationName, string(spec))
	if err := os.WriteFile(filepath.Join(configDir(), configurationFile), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// setSystemSecret writes a Secret of clowder-system with one key to the
// configuration's directory, its value encrypted from stdin as clowder
// secret encrypt does; the next apply of the directory takes it. It
// returns the reference the configuration names the value by.
func setSystemSecret(t *testing.T, name, key, value string) names.SecretKeyRef {
	t.Helper()
	secret, err := clowderWithInput(strings.NewReader(value), "secret", "encrypt", names.ID(names.System, name),
		"--from-file", key+"=-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(systemSecretFile(name), []byte(secret), 0o644); err != nil {
		t.Fatal(err)
	}
	return names.SecretKeyRef{Name: name, Key: key}
}

// runs tells whether a service of a stack has a running container.
func (h host) runs(t *testing.T, stack, service string) bool {
	t.Helper()
	return h.container(t, stack, service) != ""
}

// container is the ID of a service's running container; "" if none runs.
func (h host) container(t *testing.T, stack, service string) string {
	t.Helper()
	return h.must(t, "docker ps -q --filter label=com.docker.compose.project="+project(stack)+
		" --filter label=com.docker.compose.service="+service)
}

// project is a stack's Compose project on the host: the tests' stacks are in
// the namespace default.
func project(stack string) string {
	return names.Project(names.Default, stack)
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

// clusterMetric reads a metric that only the leader has (the room of each
// host), for one host, through server: the leader may be any host, and
// server reaches each host's /metrics on its node address.
func clusterMetric(t *testing.T, server host, name, hostName string) float64 {
	t.Helper()
	sample := name + `{host="` + hostName + `"}`
	for _, other := range clusterHosts(t) {
		if other.Phase == "removed" {
			continue
		}
		// the node address of host n (network.NodeAddress)
		url := fmt.Sprintf("http://10.77.0.%d:7444/metrics", other.ID+1)
		output, err := server.run("curl -sf " + url)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(output, "\n") {
			if fields := strings.Fields(line); len(fields) == 2 && fields[0] == sample {
				value, err := strconv.ParseFloat(fields[1], 64)
				if err != nil {
					t.Fatalf("%s: %v", line, err)
				}
				return value
			}
		}
	}
	t.Fatalf("no host's /metrics has %s", sample)
	return 0
}
