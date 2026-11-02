package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"syscall"
	"time"

	"github.com/webcodingcat/clowder/internal/api"
	"github.com/webcodingcat/clowder/internal/controller"
)

const (
	// readHeaderTimeout is how long the agent's HTTP servers wait for a
	// request's headers.
	readHeaderTimeout = 10 * time.Second
	// apiIdleTimeout is how long the API keeps an idle keep-alive
	// connection of a CLI or an agent.
	apiIdleTimeout = 2 * time.Minute
)

// serve starts the API on the host's addresses, the answers to ACME's
// challenges and the metrics. If one cannot listen, it closes those it
// started and returns that error, which ends the agent at its start. Once
// they serve, only the API's errors reach serveErrors: without the other
// two the host still runs its stacks, so they log theirs.
func (d *daemon) serve() ([]*http.Server, <-chan error, error) {
	servers, serveErrors, err := serveAPI(d.server, d.host.apiAddresses(), d.logger)
	if err != nil {
		return nil, nil, err
	}
	source := &metricsSource{
		host: d.host, cluster: d.node, docker: d.parts.Docker, restarts: d.supervisor.Restarts,
		failures: d.alerts.Failures, memory: readRSS, logger: d.logger,
	}
	for _, start := range []func() (*http.Server, error){
		func() (*http.Server, error) { return serveChallenges(d.node, d.logger) },
		func() (*http.Server, error) { return serveMetrics(source, metricsAddress(d.host), d.logger) },
	} {
		started, err := start()
		if err != nil {
			for _, server := range servers {
				_ = server.Close() // the listen error is the one to report
			}
			return nil, nil, err
		}
		servers = append(servers, started)
	}
	return servers, serveErrors, nil
}

// freeBind lets a socket bind an address the host does not have yet: at
// boot the public address can come up after the agent (DHCP, netplan), and
// the agent must neither fail nor leave users locked out then.
var freeBind = net.ListenConfig{
	Control: func(_, _ string, raw syscall.RawConn) error {
		var optErr error
		err := raw.Control(func(fd uintptr) {
			optErr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_FREEBIND, 1)
		})
		return errors.Join(err, optErr)
	},
}

// serveAPI listens on every address and serves the API there.
func serveAPI(server *api.Server, addresses []string, logger *slog.Logger) ([]*http.Server, <-chan error, error) {
	var servers []*http.Server
	serveErrors := make(chan error, len(addresses))
	for _, address := range addresses {
		listener, err := freeBind.Listen(context.Background(), "tcp", address)
		if err != nil {
			for _, started := range servers {
				_ = started.Close() // the listen error is the one to report
			}
			return nil, nil, fmt.Errorf("listen for the API on %s: %w", address, err)
		}
		httpServer := &http.Server{
			Handler:           server,
			TLSConfig:         server.TLSConfig(),
			ReadHeaderTimeout: readHeaderTimeout,
			IdleTimeout:       apiIdleTimeout,
			ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelDebug),
		}
		servers = append(servers, httpServer)
		go func() {
			if err := httpServer.ServeTLS(listener, "", ""); !errors.Is(err, http.ErrServerClosed) {
				serveErrors <- fmt.Errorf("serve the API on %s: %w", address, err)
			}
		}()
	}
	return servers, serveErrors, nil
}

// alertsAPI answers the API's alert routes from the alerts loop.
type alertsAPI struct {
	alerts *controller.Alerts
}

func (a alertsAPI) Test(ctx context.Context) ([]api.AlertTestResult, error) {
	results, err := a.alerts.Test(ctx)
	answers := []api.AlertTestResult{}
	for _, result := range results {
		answer := api.AlertTestResult{Channel: result.Channel}
		if result.Err != nil {
			answer.Error = result.Err.Error()
		}
		answers = append(answers, answer)
	}
	return answers, err
}

func (a alertsAPI) Failures() []api.AlertFailure {
	answers := []api.AlertFailure{}
	for _, failure := range a.alerts.Failures() {
		answers = append(answers, api.AlertFailure{
			Channel: failure.Channel, Key: failure.Key, Event: failure.Event, Attempts: failure.Attempts,
			LastError: failure.LastError,
		})
	}
	return answers
}
