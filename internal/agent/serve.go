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
	readHeaderTimeout = 10 * time.Second
	apiIdleTimeout    = 2 * time.Minute
)

// serve fails, and so ends the agent at its start, if any server cannot
// listen. Once they serve, only the API's errors reach serveErrors: the
// host runs its stacks without the other two servers, so they only log.
func (d *daemon) serve() ([]*http.Server, <-chan error, error) {
	servers, serveErrors, err := serveAPI(d.server, d.host.apiAddresses(), d.logger)
	if err != nil {
		return nil, nil, err
	}
	source := &metricsSource{
		host: d.host, cluster: d.node, docker: d.parts.Docker, restarts: d.supervisor.Restarts,
		failures: d.alerts.Failures, seen: d.volumes.Seen, memory: readRSS, logger: d.logger,
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

// freeBind lets a socket bind an address that the host does not have yet.
// At boot, the public address can come up after the agent starts (DHCP,
// netplan). The agent must then neither fail nor lock users out.
var freeBind = net.ListenConfig{
	Control: func(_, _ string, raw syscall.RawConn) error {
		var optErr error
		err := raw.Control(func(fd uintptr) {
			optErr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_FREEBIND, 1)
		})
		return errors.Join(err, optErr)
	},
}

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
