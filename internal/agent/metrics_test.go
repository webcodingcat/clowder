package agent

import (
	"bytes"
	"context"
	"flag"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/webcodingcat/clowder/internal/agent/agenttest"
	"github.com/webcodingcat/clowder/internal/controller"
	"github.com/webcodingcat/clowder/internal/docker"
	"github.com/webcodingcat/clowder/internal/state"
	"github.com/webcodingcat/clowder/internal/storage"
	"github.com/webcodingcat/clowder/internal/supervisor"
)

var update = flag.Bool("update", false, "write the golden files anew")

// leadingCluster is a test cluster whose agent leads or not.
type leadingCluster struct {
	*agenttest.Cluster
	leads bool
}

func (c leadingCluster) IsLeader() bool { return c.leads }

// newTestMetrics collects host 0 of agenttest.NewCluster with a pool, two
// containers of shop, a restart, a certificate, an open alert and a failed
// delivery.
func newTestMetrics(t *testing.T, leads bool) *metricsSource {
	t.Helper()
	cluster, fake := agenttest.NewCluster(t), &agenttest.Docker{}
	cluster.Apply(state.KindReportHost, state.HostActor(0), state.ReportHost{
		Host: 0, Version: "1.0.0", Capacity: state.Resources{MemoryBytes: 4 << 30, CPUs: 2},
		Pools:           map[string]state.Pool{"vg-fast/thin": {Name: "vg-fast/thin", SizeBytes: 100 << 30, DataPercent: 42, MetadataPercent: 7}},
		RootUsedPercent: 31,
	})
	cluster.Apply(state.KindPutCertificate, state.LeaderActor, state.PutCertificate{Certificate: state.Certificate{
		Domain: "shop.example.com", Key: state.Encrypted("k"), Chain: []byte("c"),
		NotBefore: time.Unix(1_790_000_000, 0), NotAfter: time.Unix(1_797_776_000, 0),
	}})
	cluster.Apply(state.KindPutAlert, state.LeaderActor, state.PutAlert{
		Key: "root-use/node0", Kind: "root-use", Severity: "warning", Subject: "s",
	})
	labels := map[string]string{docker.ProjectLabel: agenttest.ShopProject, docker.ServiceLabel: "app"}
	fake.Listed = []docker.Container{
		{ID: "a", State: "running", Labels: labels},
		{ID: "b", State: "exited", Labels: labels},
		{ID: "t", State: "running", Labels: map[string]string{docker.ProjectLabel: "clowder-traefik", docker.ServiceLabel: "traefik"}},
	}
	return &metricsSource{
		host: Host{ID: 0, Name: "node0"}, cluster: leadingCluster{cluster, leads}, docker: fake,
		restarts: func() map[supervisor.Service]int {
			return map[supervisor.Service]int{{Stack: agenttest.ShopProject, Name: "app"}: 3}
		},
		failures: func() []controller.Failure { return []controller.Failure{{Key: "root-use/node0"}} },
		seen: func() map[string]storage.Observed {
			return map[string]storage.Observed{
				"default/db":    {Role: storage.RolePrimary, Disk: "Inconsistent", Quorum: true, OutOfSyncBytes: 4096},
				"default/files": {},
			}
		},
		memory: func() (int64, error) { return 41 << 20, nil },
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// golden compares rendered with testdata/<name>; -update writes it anew.
func golden(t *testing.T, name string, rendered []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, rendered, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run the test with -update to write it)", err)
	}
	if string(rendered) != string(want) {
		t.Errorf("differs from %s; run with -update and review the diff:\n%s", path, rendered)
	}
}

// scrape collects and writes one scrape.
func scrape(t *testing.T, source *metricsSource) []byte {
	t.Helper()
	set := source.collect(context.Background())
	var out bytes.Buffer
	if err := set.Write(&out); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestTheLeadersMetricsMatchTheGoldenFile(t *testing.T) {
	golden(t, "metrics-leader.golden.txt", scrape(t, newTestMetrics(t, true)))
}

func TestAFollowerExportsOnlyWhatIsAboutItsHost(t *testing.T) {
	out := bytes.NewBuffer(scrape(t, newTestMetrics(t, false)))
	golden(t, "metrics-follower.golden.txt", out.Bytes())
	for _, leaders := range []string{"clowder_alerts_open", "clowder_certificate_expiry_seconds", "clowder_host_offered_memory_bytes"} {
		if bytes.Contains(out.Bytes(), []byte(leaders)) {
			t.Errorf("a follower exports %s", leaders)
		}
	}
}
