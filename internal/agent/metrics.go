package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/webcodingcat/clowder/internal/agent/loop"
	"github.com/webcodingcat/clowder/internal/clusterspec"
	"github.com/webcodingcat/clowder/internal/compose"
	"github.com/webcodingcat/clowder/internal/controller"
	"github.com/webcodingcat/clowder/internal/docker"
	"github.com/webcodingcat/clowder/internal/metrics"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/scheduler"
	"github.com/webcodingcat/clowder/internal/state"
	"github.com/webcodingcat/clowder/internal/storage"
	"github.com/webcodingcat/clowder/internal/supervisor"
)

// MetricsPort is the port where every agent serves /metrics, on its node
// address only. The cluster's hosts reach it through WireGuard. Containers
// cannot reach it (the nft table), and it needs no authentication. Unlike
// the Raft and API ports, it is not a field of the host, because Prometheus
// scrapes every host on the same port.
const MetricsPort = 7444

// metricsTimeout limits the scrape's calls to Docker, well below
// Prometheus' default scrape timeout of 10 s, so that a slow Docker costs
// only the containers' values, not the whole scrape.
const metricsTimeout = 5 * time.Second

// metricsSource gathers what /metrics shows. Each agent exports the values
// of its own host. Only the leader exports the cluster-wide values (open
// alerts, failed deliveries, certificates, the scheduler's room per host),
// so that a Prometheus that scrapes every host counts them once.
type metricsSource struct {
	host    Host
	cluster interface {
		loop.Cluster
		IsLeader() bool
	}
	docker   Docker
	restarts func() map[supervisor.Service]int
	failures func() []controller.Failure
	// seen is volumes.Reconciler.Seen
	seen func() map[string]storage.Observed
	// memory is readRSS; tests replace it
	memory func() (int64, error)
	logger *slog.Logger
}

func (m *metricsSource) collect(ctx context.Context) *metrics.Set {
	set := metrics.NewSet()
	// a copy instead of the state under its lock: the scrape waits for
	// Docker, and meanwhile Raft's Apply would wait for the lock
	copied := m.cluster.SharedCopy()
	leads := m.cluster.IsLeader()
	m.addHost(set, copied, leads)
	m.addContainers(ctx, set)
	if leads {
		m.addCluster(set, copied)
	}
	return set
}

func (m *metricsSource) addHost(set *metrics.Set, copied *state.State, leads bool) {
	name := m.host.Name
	set.Add("clowder_raft_applied_index", "Raft index of the last command this host applied.", metrics.Gauge,
		float64(copied.Index), "host", name)
	set.Add("clowder_leader", "1 while this host leads the cluster.", metrics.Gauge, boolValue(leads), "host", name)
	if memory, err := m.memory(); err == nil {
		set.Add("clowder_agent_memory_bytes", "Resident memory of clowderd.", metrics.Gauge, float64(memory), "host", name)
	}
	host, ok := copied.Hosts[m.host.ID]
	if !ok {
		return
	}
	for _, phase := range state.Phases {
		set.Add("clowder_host_phase", "The host's phase: 1 for the one it is in, 0 for the others.", metrics.Gauge,
			boolValue(host.Phase == phase), "host", name, "phase", string(phase))
	}
	set.Add("clowder_docker_up", "1 while Docker answers the agent's ping.", metrics.Gauge, boolValue(!host.DockerDown),
		"host", name)
	set.Add("clowder_root_used_percent", "How full the root file system is.", metrics.Gauge,
		float64(host.RootUsedPercent), "host", name)
	for _, pool := range slices.Sorted(maps.Keys(host.Pools)) {
		set.Add("clowder_pool_data_percent", "How full a thin pool's data is.", metrics.Gauge,
			float64(host.Pools[pool].DataPercent), "host", name, "pool", pool)
		set.Add("clowder_pool_metadata_percent", "How full a thin pool's metadata is.", metrics.Gauge,
			float64(host.Pools[pool].MetadataPercent), "host", name, "pool", pool)
	}
	m.addCopies(set)
}

func (m *metricsSource) addCopies(set *metrics.Set) {
	name := m.host.Name
	seen := m.seen()
	for _, volume := range slices.Sorted(maps.Keys(seen)) {
		copyNow := seen[volume]
		set.Add("clowder_volume_out_of_sync_bytes", "How much this host's copy of a volume differs from a peer: while it resyncs, what is left.",
			metrics.Gauge, float64(copyNow.OutOfSyncBytes), "host", name, "volume", volume)
		if copyNow.Role == storage.RoleDown {
			continue
		}
		set.Add("clowder_volume_primary", "1 while this host's copy of a volume is DRBD's primary.", metrics.Gauge,
			boolValue(copyNow.Role == storage.RolePrimary), "host", name, "volume", volume)
		set.Add("clowder_volume_up_to_date", "1 while this host's copy of a volume holds its current data (a tiebreaker holds none).",
			metrics.Gauge, boolValue(copyNow.Disk == storage.DiskUpToDate), "host", name, "volume", volume)
		set.Add("clowder_volume_quorum", "1 while this host's copy of a volume has DRBD's quorum; a primary without it suspends its writes.",
			metrics.Gauge, boolValue(copyNow.Quorum), "host", name, "volume", volume)
		set.Add("clowder_volume_split_brain", "1 while a peer stands alone from this host's copy of a volume: a split brain for a human.",
			metrics.Gauge, boolValue(copyNow.SplitBrain), "host", name, "volume", volume)
	}
}

func (m *metricsSource) addContainers(ctx context.Context, set *metrics.Set) {
	ctx, cancel := context.WithTimeout(ctx, metricsTimeout)
	defer cancel()
	containers, err := m.docker.Containers(ctx, compose.LabelStack)
	set.Add("clowder_containers_listed", "1 when the scrape could list the containers, so their values are complete.",
		metrics.Gauge, boolValue(err == nil), "host", m.host.Name)
	if err != nil {
		m.logger.Warn("collect the metrics: list the containers", "error", err)
	}
	// clowder's own Traefik counts too: its containers and restarts show
	// the host's health just as a stack's do
	counts := map[[3]string]int{}
	for _, container := range containers {
		counts[[3]string{container.Labels[docker.ProjectLabel], container.Labels[docker.ServiceLabel], container.State}]++
	}
	for key, count := range counts {
		namespace, stack := stackLabels(key[0])
		set.Add("clowder_containers", "Containers of the stacks on this host, by state.", metrics.Gauge, float64(count),
			"host", m.host.Name, "namespace", namespace, "stack", stack, "service", key[1], "state", key[2])
	}
	for service, count := range m.restarts() {
		namespace, stack := stackLabels(service.Stack)
		set.Add("clowder_container_restarts_total",
			"Restarts the supervisor began for a service's containers (crashed or unhealthy) since clowderd started.",
			metrics.Counter, float64(count), "host", m.host.Name, "namespace", namespace, "stack", stack,
			"service", service.Name)
	}
}

// stackLabels keeps the project name of clowder's own projects (Traefik),
// which have no namespace.
func stackLabels(project string) (namespace, stack string) {
	if namespace, stack, ok := names.SplitProject(project); ok {
		return namespace, stack
	}
	return "", project
}

func (m *metricsSource) addCluster(set *metrics.Set, copied *state.State) {
	rooms := scheduler.Rooms(copied)
	for _, id := range slices.Sorted(maps.Keys(rooms)) {
		room, name := rooms[id], copied.Hosts[id].Name
		set.Add("clowder_host_offered_memory_bytes", "Memory a host offers its services: its memory less what it keeps for itself.",
			metrics.Gauge, float64(room.OfferedMemory), "host", name)
		set.Add("clowder_host_reserved_memory_bytes", "Memory the services on a host reserve.",
			metrics.Gauge, float64(room.ReservedMemory), "host", name)
		set.Add("clowder_host_offered_cpus", "CPUs a host offers its services: its CPUs less what it keeps for itself.",
			metrics.Gauge, float64(room.OfferedMilliCPUs)/1000, "host", name)
		set.Add("clowder_host_reserved_cpus", "CPUs the services on a host reserve.",
			metrics.Gauge, float64(room.ReservedMilliCPUs)/1000, "host", name)
	}
	for domain, certificate := range copied.Certificates {
		set.Add("clowder_certificate_expiry_seconds", "When a route's certificate ends, in Unix seconds.",
			metrics.Gauge, float64(certificate.NotAfter.Unix()), "domain", domain)
	}
	severities := map[string]int{}
	for _, alert := range copied.Alerts {
		severities[alert.Severity]++
	}
	for _, severity := range clusterspec.Severities {
		set.Add("clowder_alerts_open", "Open alerts by severity.", metrics.Gauge, float64(severities[severity]),
			"severity", severity)
	}
	set.Add("clowder_alert_deliveries_failed", "Alert messages a channel has not taken yet.", metrics.Gauge,
		float64(len(m.failures())))
}

func boolValue(value bool) float64 {
	if value {
		return 1
	}
	return 0
}

// readRSS returns the agent's resident memory in bytes. The live test
// measures the agent's RAM with it.
func readRSS() (int64, error) {
	file, err := os.Open("/proc/self/status")
	if err != nil {
		return 0, err
	}
	defer func() { _ = file.Close() }() // read only
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		// "VmRSS:	   41236 kB"
		fields := strings.Fields(scanner.Text())
		if len(fields) == 3 && fields[0] == "VmRSS:" && fields[2] == "kB" {
			kibibytes, err := strconv.ParseInt(fields[1], 10, 64)
			return kibibytes * 1024, err
		}
	}
	return 0, errors.New("no VmRSS in /proc/self/status")
}

func serveMetrics(source *metricsSource, address string, logger *slog.Logger) (*http.Server, error) {
	listener, err := freeBind.Listen(context.Background(), "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("listen for /metrics on %s: %w", address, err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		set := source.collect(r.Context())
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_ = set.Write(w) // the scraper is gone if this fails
	})
	server := &http.Server{
		Handler: mux, ReadHeaderTimeout: readHeaderTimeout,
		ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelDebug),
	}
	go func() {
		if err := server.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
			logger.Error("serve /metrics", "error", err)
		}
	}()
	return server, nil
}

func metricsAddress(host Host) string {
	return net.JoinHostPort(host.NodeAddress.String(), strconv.Itoa(MetricsPort))
}
