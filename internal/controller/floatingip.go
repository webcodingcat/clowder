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

	"github.com/webcodingcat/clowder/internal/hetzner"
	"github.com/webcodingcat/clowder/internal/state"
)

// hetznerTimeout bounds one look at Hetzner's API, an assignment included.
const hetznerTimeout = 2 * time.Minute

// RunFloatingIP keeps the Floating IP on the ingress host while this agent
// leads, until ctx ends: at a change of the state and every interval, but
// it reads Hetzner's API at most once per interval (Hetzner allows 3600
// requests an hour; this takes about 120). While it fails, it observes the
// condition ingressNotHeld, which the alerts turn into an alert. Traefik,
// which answers on the Floating IP, is internal/ingress's.
func RunFloatingIP(ctx context.Context, cluster Cluster, propose Propose, open Open, conditions *Conditions,
	logger *slog.Logger,
) {
	var next time.Time // when the API may be read again
	for {
		changed := cluster.Changed()
		if cluster.IsLeader() && !time.Now().Before(next) {
			asked, err := holdFloatingIP(ctx, cluster, propose, open, logger)
			if asked {
				next = time.Now().Add(interval)
			}
			if err != nil {
				logger.Warn("the Floating IP is not on the ingress host", "error", err)
			}
			// a pass that did not ask Hetzner proves nothing, unless it found
			// no host to hold the Floating IP
			if asked || err != nil {
				conditions.Observe(ingressNotHeld, err != nil)
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

// holdFloatingIP records the ingress host when it changes, then routes the
// Floating IP to it if Hetzner has it elsewhere (moved by hand, or the host
// is new). It tells whether it asked Hetzner's API, which counts against
// the API's limit.
func holdFloatingIP(ctx context.Context, cluster Cluster, propose Propose, open Open, logger *slog.Logger,
) (asked bool, err error) {
	copied, err := cluster.SharedCopy()
	if err != nil {
		return false, err
	}
	config := copied.Config.Spec
	if copied.Config.Revision == 0 {
		return false, nil // no cluster.yaml yet: no Floating IP and no token to ask with
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
	// a leader cut off from the others must not take the address from the
	// host the new leader routes it to
	if err := cluster.VerifyLeader(ctx); err != nil {
		return true, err
	}
	logger.Info("routing the Floating IP to the ingress host", "host", host.Name, "server", host.ServerID, "was", target)
	return true, client.AssignFloatingIP(ctx, ip, host.ServerID)
}

// confirmServer checks a host's server with Hetzner and records it as
// checked. The first host's server ID came from its metadata service
// unchecked; a joined host's was checked as it joined. A server Hetzner
// does not confirm keeps the Floating IP off the host, and the loop's
// condition (ingressNotHeld) raises the alert.
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

// OpenHetzner makes the client of Hetzner's API with the token of
// cluster.yaml opened; the leader's loops, the join and recover use it.
func OpenHetzner(current *state.State, open Open) (*hetzner.Client, error) {
	config := current.Config.Spec
	sealed, ok := current.ClusterSecret(config.Hetzner.Token)
	if !ok {
		return nil, fmt.Errorf("the Hetzner token %s is not among the cluster's secrets", config.Hetzner.Token)
	}
	token, err := open(sealed)
	if err != nil {
		return nil, fmt.Errorf("open the Hetzner token: %w", err)
	}
	return hetzner.New(config.Hetzner.API, string(token)), nil
}

// ingressHost is the host that should hold the Floating IP: the recorded
// one while it can (no switching back), else the first of
// ingress.preferred_hosts that can, else the one with the lowest ID that
// can. A host can when it is active and Hetzner confirmed its server. Only
// while no such host is there does a host with a server not checked yet
// count (the first host, before its check in holdFloatingIP): so a server
// Hetzner refuses keeps the Floating IP from no host that has a checked
// one.
func ingressHost(current *state.State) (*state.Host, bool) {
	if host, ok := chooseIngress(current, true); ok {
		return host, true
	}
	return chooseIngress(current, false)
}

// chooseIngress is ingressHost's choice among the active hosts, and with
// checked servers only if checkedOnly says so.
func chooseIngress(current *state.State, checkedOnly bool) (*state.Host, bool) {
	canHold := func(host *state.Host) bool {
		return host != nil && host.Phase == state.Active && (host.ServerChecked || !checkedOnly)
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
