//go:build live

package live

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/webcodingcat/clowder/internal/clusterspec"
)

// The mail server bin/dev live runs in the lab's network: STARTTLS with the
// registry's certificate, any login, and an API to read what came.
const (
	mailServerPort = 1025
	mailAPI        = "http://" + labAddress + ":8025/api/v1/messages"
	mailCAFile     = "mail-ca.pem"
)

// The image of Prometheus and promtool, run on the host.
const prometheusImage = "prom/prometheus:v3.5.0"

// heartbeatInterval is the test's alerts.heartbeat.interval: short, so a
// stop shows in seconds.
const heartbeatInterval = 10 * time.Second

// alertsReachTheChannels sets up the three channels and the heartbeat: a
// test alert reaches each, the heartbeat stops while the agent is stopped,
// and a pool that fills raises its alert and the all-clear.
func alertsReachTheChannels(t *testing.T, server host) {
	received := startReceiver(t)
	caCert, err := os.ReadFile(filepath.Join(workDir, registryCAFile))
	if err != nil {
		t.Fatal(err)
	}
	writeClusterFile(t, mailCAFile, caCert)
	setClusterSecrets(t, map[string]string{
		"alerts.channels[0].webhook": received.url("/discord"), "alerts.channels[1].url": received.url("/webhook"),
		"alerts.channels[2].smtp.password": "any", "alerts.heartbeat.url": received.url("/heartbeat"),
	})
	changeConfig(t, func(config *clusterspec.Config) {
		config.Alerts = &clusterspec.Alerts{
			Channels: []clusterspec.AlertChannel{
				{Type: "discord", Webhook: "alerts.channels[0].webhook"},
				{Type: "webhook", URL: "alerts.channels[1].url"},
				{
					Type: "email", From: "clowder@" + zone, To: []string{"ops@" + zone},
					SMTP: &clusterspec.SMTP{
						Host: labAddress, Port: mailServerPort, User: "clowder",
						Password: "alerts.channels[2].smtp.password", CA: mailCAFile,
					},
				},
			},
			Heartbeat: &clusterspec.Heartbeat{URL: "alerts.heartbeat.url", Interval: heartbeatInterval.String()},
		}
	})

	// alert test sends before it answers: what came is there now
	mustClowder(t, "alert", "test")
	if len(received.got("/discord")) != 1 || !received.hasMessage("/webhook", "test", "test") {
		t.Errorf("the test alert: discord %v, webhook %v", received.got("/discord"), received.got("/webhook"))
	}
	if subjects := mailSubjects(t); !strings.Contains(subjects, "[live] info: test alert") {
		t.Errorf("no test alert in the mail: %s", subjects)
	}

	heartbeat(t, server, received)
	fillPool(t, server, received)
	// the receiver ends with this step: without the channels, the later
	// steps' alerts fail no delivery
	changeConfig(t, func(config *clusterspec.Config) { config.Alerts = nil })
}

// mailSubjects are the subjects of the mail that came, one per line.
func mailSubjects(t *testing.T) string {
	t.Helper()
	var answer struct {
		Messages []struct{ Subject string } `json:"messages"`
	}
	if err := json.Unmarshal(download(t, mailAPI), &answer); err != nil {
		t.Fatal(err)
	}
	var subjects []string
	for _, message := range answer.Messages {
		subjects = append(subjects, message.Subject)
	}
	return strings.Join(subjects, "\n")
}

// heartbeat: the leader calls the heartbeat every 10 s; with the agent
// stopped the calls stop, started again they come back.
func heartbeat(t *testing.T, server host, received *receiver) {
	eventually(t, time.Minute, "the heartbeat comes", func() error {
		if calls := len(received.got("/heartbeat")); calls < 2 {
			return fmt.Errorf("%d calls", calls)
		}
		return nil
	})
	server.must(t, "systemctl stop clowderd")
	// started again below; a failure before must not leave it stopped for
	// the diagnosis
	t.Cleanup(func() { _, _ = server.run("systemctl start clowderd") })
	stopped := len(received.got("/heartbeat"))
	time.Sleep(4 * heartbeatInterval) // four calls missed if it went on
	// one call may have been on its way
	if calls := len(received.got("/heartbeat")); calls > stopped+1 {
		t.Errorf("%d heartbeats while the agent was stopped", calls-stopped)
	}
	server.must(t, "systemctl start clowderd")
	again := len(received.got("/heartbeat"))
	eventually(t, 2*time.Minute, "the heartbeat comes back", func() error {
		if calls := len(received.got("/heartbeat")); calls <= again {
			return fmt.Errorf("no call since the start")
		}
		return nil
	})
}

// fillPool fills the small storage class's pool beyond 80 %: the alert
// pool-use comes, and its all-clear once the volume is gone.
func fillPool(t *testing.T, server host, received *receiver) {
	// a reserve of 5 %, so one volume can fill the pool beyond 80 %
	changeConfig(t, func(config *clusterspec.Config) {
		small := config.StorageClasses["small"]
		small.Reserve = "5%"
		config.StorageClasses["small"] = small
	})
	poolBytes, err := strconv.ParseInt(server.must(t,
		"lvs --noheadings --units b --nosuffix -o lv_size vg-small/thin"), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	createAndApply(t, "filler", simpleStack("filler", "filler-data")+
		volumeDocument("filler-data", "small", fmt.Sprintf("%dM", poolBytes*90/100>>20), 0))
	waitRunning(t, "filler", 1)
	app := server.must(t, "docker ps -q --filter label=com.docker.compose.project="+project("filler"))
	// zeros take blocks of a thin pool as any data does
	server.must(t, fmt.Sprintf("docker exec %s dd if=/dev/zero of=/data/fill bs=1M count=%d conv=fsync 2>&1 | tail -1",
		app, poolBytes*85/100>>20))
	// the alert's key is pool-use/<host>/<pool>/data
	key := "vg-small/thin/data"
	eventually(t, 3*time.Minute, "the alert pool-use", func() error {
		if !strings.Contains(clusterStatus(t), key) || !received.hasMessage("/webhook", "raised", key) {
			return fmt.Errorf("not raised and sent")
		}
		return nil
	})
	// the stack, then its volume with the data
	removeStack(t, "filler")
	eventually(t, 3*time.Minute, "the all-clear of pool-use", func() error {
		if !received.hasMessage("/webhook", "resolved", key) {
			return fmt.Errorf("no all-clear")
		}
		return nil
	})
}

// scrapeMetrics checks /metrics with promtool, then lets a Prometheus on the
// host itself (the only kind that reaches the node address in phase 1b)
// scrape it.
func scrapeMetrics(t *testing.T, server host) {
	server.must(t, "docker pull -q "+prometheusImage)
	server.must(t, "curl -sf "+metricsURL+" | docker run --rm -i --entrypoint promtool "+prometheusImage+" check metrics")
	config := "scrape_configs:\n  - job_name: clowder\n    scrape_interval: 5s\n" +
		"    static_configs:\n      - targets: [\"" + metricsTarget + "\"]\n"
	if _, err := server.runWithInput("mkdir -p /tmp/live-prometheus && cat > /tmp/live-prometheus/prometheus.yml",
		strings.NewReader(config)); err != nil {
		t.Fatal(err)
	}
	server.must(t, "docker run -d --name live-prometheus --network host -v /tmp/live-prometheus:/etc/prometheus:ro "+
		prometheusImage+" --config.file=/etc/prometheus/prometheus.yml --web.listen-address=127.0.0.1:9090")
	defer func() { _, _ = server.run("docker rm -f live-prometheus") }() // the test's own container
	eventually(t, time.Minute, "Prometheus scrapes the agent", func() error {
		output, err := server.run("curl -sf 'http://127.0.0.1:9090/api/v1/query?query=up'")
		if err != nil {
			return err
		}
		var answer struct {
			Data struct {
				Result []struct {
					Value []any `json:"value"`
				} `json:"result"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(output), &answer); err != nil {
			return err
		}
		if len(answer.Data.Result) != 1 || len(answer.Data.Result[0].Value) != 2 || answer.Data.Result[0].Value[1] != "1" {
			return fmt.Errorf("up: %s", output)
		}
		return nil
	})
}
