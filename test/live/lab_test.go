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

	"github.com/webcodingcat/clowder/internal/clusterdir"
	"github.com/webcodingcat/clowder/internal/clusterspec"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/secretvalue"
	"github.com/webcodingcat/clowder/internal/yamlpath"
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

// changeConfig edits the test's cluster.yaml and applies it.
func changeConfig(t *testing.T, change func(config *clusterspec.Config)) {
	t.Helper()
	directory, err := clusterdir.Load(clusterDir())
	if err != nil {
		t.Fatal(err)
	}
	change(&directory.Config)
	writeConfig(t, directory.Config, directory.Encrypted)
	mustClowder(t, "cluster", "apply")
}

// clusterConfigJSON is the test's cluster.yaml as JSON: its settings, with
// the names of its secrets and not their values, which change at each
// encrypt.
func clusterConfigJSON(t *testing.T) []byte {
	t.Helper()
	directory, err := clusterdir.Load(clusterDir())
	if err != nil {
		t.Fatal(err)
	}
	content, err := json.MarshalIndent(directory.Config, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return content
}

// encrypted are the values of cluster.yaml the test set, by name (their
// path), encrypted as clowder secret encrypt does.
var encrypted = map[string]string{}

// setClusterSecrets encrypts values for cluster.yaml, by their names; the
// next writeConfig puts them at their places.
func setClusterSecrets(t *testing.T, values map[string]string) {
	t.Helper()
	for name, value := range values {
		block, err := clowderWithInput(strings.NewReader(value), "secret", "encrypt", "--cluster-yaml")
		if err != nil {
			t.Fatal(err)
		}
		// the armor without the block's tag line and indentation
		_, armor, _ := strings.Cut(block, "\n")
		var lines []string
		for _, line := range strings.Split(strings.TrimRight(armor, "\n"), "\n") {
			lines = append(lines, strings.TrimSpace(line))
		}
		encrypted[name] = strings.Join(lines, "\n") + "\n"
	}
}

// writeConfig writes the test's cluster.yaml: config, with the value of
// each of its secrets tagged !secret at its place, from known (what the file
// had) or set by setClusterSecrets.
func writeConfig(t *testing.T, config clusterspec.Config, known map[string]string) {
	t.Helper()
	values := map[string]string{}
	for _, place := range clusterspec.SecretPlaces(config) {
		value, ok := encrypted[place.Path]
		if !ok {
			value, ok = known[place.Path]
		}
		if !ok {
			t.Fatalf("no value for %s", place.Path)
		}
		values[place.Path] = value
	}
	var document yaml.Node
	if err := document.Encode(config); err != nil {
		t.Fatal(err)
	}
	tagSecrets(&document, "", values)
	content, err := yaml.Marshal(&document)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clusterDir(), clusterdir.ConfigFile), content, 0o644); err != nil {
		t.Fatal(err)
	}
}

// tagSecrets puts each value at its path in the node tree, as a block
// tagged !secret, naming the places as the CLI's Extract does.
func tagSecrets(node *yaml.Node, path string, values map[string]string) {
	switch node.Kind {
	case yaml.MappingNode:
		for index := 0; index+1 < len(node.Content); index += 2 {
			tagSecrets(node.Content[index+1], yamlpath.Key(path, node.Content[index].Value), values)
		}
	case yaml.SequenceNode:
		for index, item := range node.Content {
			tagSecrets(item, yamlpath.Index(path, index), values)
		}
	case yaml.ScalarNode:
		if value, ok := values[path]; ok {
			node.Tag, node.Style, node.Value = secretvalue.Tag, yaml.LiteralStyle, value
		}
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
