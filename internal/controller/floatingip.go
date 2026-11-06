package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/netip"
	"slices"
	"time"

	"github.com/webcodingcat/clowder/internal/clusterspec"
	"github.com/webcodingcat/clowder/internal/hetzner"
	"github.com/webcodingcat/clowder/internal/hostrole"
	"github.com/webcodingcat/clowder/internal/state"
)

// hetznerTimeout bounds one look at Hetzner's API, an assignment included.
const hetznerTimeout = 2 * time.Minute

// RunFloatingIP keeps the Floating IP on the ingress host while this agent
// leads, until ctx ends. It looks at every change of the state and every
// interval, but it reads Hetzner's API at most once per interval: Hetzner
// allows 3600 requests an hour, and this loop takes about 120. While it
// fails, it observes the condition ingressNotHeld, which the alerts turn
// into an alert. Traefik, which answers on the Floating IP, belongs to
// internal/ingress.
func RunFloatingIP(ctx context.Context, cluster Cluster, propose Propose, open Open, conditions *Conditions,
	logger *slog.Logger,
) {
	var next time.Time // when the API may be read again
	var warned []string
	for {
		changed := cluster.Changed()
		if cluster.IsLeader() {
			// warn here, because the leader is the one that talks to
			// Hetzner's API and the ACME CA
			warned = warnServiceURLs(cluster, warned, logger)
		}
		if cluster.IsLeader() && !time.Now().Before(next) {
			asked, err := holdFloatingIP(ctx, cluster, propose, open, logger)
			if asked {
				next = time.Now().Add(interval)
			}
			if err != nil {
				logger.Warn("the Floating IP is not on the ingress host", "error", err)
			}
			// a pass that did not ask Hetzner proves nothing, unless it
			// found no host to hold the Floating IP
			if asked || err != nil {
				conditions.Observe(ingressNotHeld, err != nil)
			}
			if asked {
				conditions.Observe(hetznerTokenRefused, hetzner.TokenRefused(err))
			}
		}
		wait := interval
		if untilNext := time.Until(next); untilNext > 0 && untilNext < interval {
			wait = untilNext // a change came early: look once the API may be read again
		}
		select {
		case <-ctx.Done():
			return
		case <-changed:
		case <-time.After(wait):
		}
	}
}

// warnServiceURLs logs clusterspec.ServiceURLWarnings when they differ
// from the warnings it logged before, and returns them. So it logs them
// once for each configuration that has them, not at every pass.
func warnServiceURLs(cluster Cluster, logged []string, logger *slog.Logger) []string {
	var warnings []string
	cluster.Read(func(current *state.State) { warnings = clusterspec.ServiceURLWarnings(current.Config.Spec) })
	if !slices.Equal(warnings, logged) {
		for _, warning := range warnings {
			logger.Warn(warning)
		}
	}
	return warnings
}

// holdFloatingIP records the ingress host when it changes. Then it routes
// the Floating IP to that host if Hetzner routes it elsewhere, because it
// was moved by hand or the host is new. It tells whether it asked Hetzner's
// API, since each request counts against the API's limit.
func holdFloatingIP(ctx context.Context, cluster Cluster, propose Propose, open Open, logger *slog.Logger,
) (asked bool, err error) {
	copied, err := cluster.SharedCopy()
	if err != nil {
		return false, err
	}
	config := copied.Config.Spec
	if !copied.Configured() {
		return false, nil // no configuration yet: no Floating IP and no token to ask with
	}
	host, ok := ingressHost(copied)
	if !ok {
		return false, errors.New("no host can hold it: none is active")
	}
	ip, err := netip.ParseAddr(config.Ingress.FloatingIP)
	if err != nil {
		return false, fmt.Errorf("read the Floating IP: %w", err)
	}
	client, err := OpenHetzner(copied, open)
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(ctx, hetznerTimeout)
	defer cancel()
	if !host.ServerChecked {
		if err := confirmServer(ctx, client, propose, host, logger); err != nil {
			return true, err
		}
	}
	if copied.Ingress == nil || *copied.Ingress != host.ID {
		logger.Info("the ingress moves", "host", host.Name)
		if _, err := commit(ctx, propose, state.KindSetIngress, state.SetIngress{Host: host.ID}); err != nil {
			return true, fmt.Errorf("record the ingress host: %w", err)
		}
	}

	target, err := client.FloatingIPTarget(ctx, ip)
	if err != nil {
		return true, err
	}
	if target == host.ServerID {
		return true, nil
	}
	// a leader that is cut off from the others must not take the address
	// from the host that the new leader routes it to
	if err := cluster.VerifyLeader(ctx); err != nil {
		return true, err
	}
	logger.Info("routing the Floating IP to the ingress host", "host", host.Name, "server", host.ServerID, "was", target)
	return true, client.AssignFloatingIP(ctx, ip, host.ServerID)
}

// confirmServer checks a host's server with Hetzner and records it as
// checked. The first host's server ID came unchecked from its metadata
// service; a joined host's server ID was checked when it joined. If Hetzner
// does not confirm the server, the Floating IP stays off the host, and the
// loop's condition (ingressNotHeld) raises the alert.
func confirmServer(ctx context.Context, client *hetzner.Client, propose Propose, host *state.Host,
	logger *slog.Logger,
) error {
	if err := client.CheckServer(ctx, host.ServerID, host.Underlay, host.PublicAddress); err != nil {
		return fmt.Errorf("host %s: %w", host.Name, err)
	}
	confirm := state.ConfirmServer{Host: host.ID, ServerID: host.ServerID}
	if _, err := commit(ctx, propose, state.KindConfirmServer, confirm); err != nil {
		return fmt.Errorf("record the checked server of host %s: %w", host.Name, err)
	}
	logger.Info("Hetzner confirmed the host's server", "host", host.Name, "server", host.ServerID)
	return nil
}

// OpenHetzner makes the client of Hetzner's API with the token that the
// configuration names, read from its Secret in clowder-system. The leader's
// loops, the join, recover and the agent's start use it.
func OpenHetzner(current *state.State, open Open) (*hetzner.Client, error) {
	encrypted, err := current.HetznerToken()
	if err != nil {
		return nil, err
	}
	token, err := open(encrypted)
	if err != nil {
		return nil, fmt.Errorf("open the Hetzner token: %w", err)
	}
	return hetzner.New(current.Config.Spec.Hetzner.API, string(token)), nil
}

// ingressHost is the host that should hold the Floating IP. It is the
// recorded host while that host can hold it (there is no switching back).
// Otherwise it is the first host of ingress.preferredHosts that can, and
// otherwise the host with the lowest ID that can. A host can hold the
// Floating IP when it is an active ingress host and Hetzner has confirmed
// its server. A host whose server is not checked yet (the first host,
// before its check in holdFloatingIP) counts only while no such host
// exists. So a server that Hetzner refuses never keeps the Floating IP from
// a host whose server is checked.
func ingressHost(current *state.State) (*state.Host, bool) {
	if host, ok := chooseIngress(current, true); ok {
		return host, true
	}
	return chooseIngress(current, false)
}

// chooseIngress makes ingressHost's choice among the active hosts. If
// checkedOnly is true, it considers only hosts with checked servers.
func chooseIngress(current *state.State, checkedOnly bool) (*state.Host, bool) {
	canHold := func(host *state.Host) bool {
		return host != nil && host.Roles.Has(hostrole.Ingress) && host.Phase == state.Active &&
			(host.ServerChecked || !checkedOnly)
	}
	if current.Ingress != nil && canHold(current.Hosts[*current.Ingress]) {
		return current.Hosts[*current.Ingress], true
	}
	ids := slices.Sorted(maps.Keys(current.Hosts))
	for _, name := range current.Config.Spec.Ingress.PreferredHosts {
		for _, id := range ids {
			if host := current.Hosts[id]; host.Name == name && canHold(host) {
				return host, true
			}
		}
	}
	for _, id := range ids {
		if host := current.Hosts[id]; canHold(host) {
			return host, true
		}
	}
	return nil, false
}
