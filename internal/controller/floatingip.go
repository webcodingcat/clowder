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
	"github.com/webcodingcat/clowder/internal/hoster"
	"github.com/webcodingcat/clowder/internal/state"
)

// ConnectHoster makes the client of the hoster cluster.yaml names, with
// its token in clear (hoster.New).
type ConnectHoster func(config clusterspec.Hoster, token string) (hoster.Hoster, error)

// hosterTimeout bounds one look at the hoster, an assignment included.
const hosterTimeout = 2 * time.Minute

// RunFloatingIP keeps the Floating IP on the ingress host while this agent
// leads, until ctx ends: at a change of the state and every interval, but
// it reads the hoster's API at most once per interval (Hetzner allows 3600
// requests an hour; this takes about 120). While it fails, it observes the
// condition ingressNotHeld, which the alerts turn into an alert. Traefik,
// which answers on the Floating IP, is internal/ingress's.
func RunFloatingIP(ctx context.Context, cluster Cluster, propose Propose, open Open, connect ConnectHoster,
	conditions *Conditions, logger *slog.Logger,
) {
	var next time.Time // when the API may be read again
	for {
		changed := cluster.Changed()
		if cluster.IsLeader() && !time.Now().Before(next) {
			asked, err := holdFloatingIP(ctx, cluster, propose, open, connect, logger)
			if asked {
				next = time.Now().Add(interval)
			}
			if err != nil {
				logger.Warn("the Floating IP is not on the ingress host", "error", err)
			}
			// a pass that did not ask the hoster proves nothing, unless it
			// found no host to hold the Floating IP
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
// Floating IP to it if the hoster has it elsewhere (moved by hand, or the
// host is new). It tells whether it asked the hoster's API, which counts
// against the API's limit.
func holdFloatingIP(ctx context.Context, cluster Cluster, propose Propose, open Open, connect ConnectHoster,
	logger *slog.Logger,
) (asked bool, err error) {
	copied, err := cluster.SharedCopy()
	if err != nil {
		return false, err
	}
	config := copied.Config.Spec
	if copied.Config.Revision == 0 {
		return false, nil // no cluster.yaml yet: no Floating IP and no hoster to ask
	}
	host, ok := ingressHost(copied)
	if !ok {
		return false, errors.New("no host can hold it: none is active and has a server ref")
	}
	ip, err := netip.ParseAddr(config.Ingress.FloatingIP)
	if err != nil {
		return false, fmt.Errorf("read the Floating IP: %w", err)
	}
	client, err := OpenHoster(copied, open, connect)
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(ctx, hosterTimeout)
	defer cancel()
	if !host.ServerRefChecked {
		if err := confirmServerRef(ctx, client, propose, host, logger); err != nil {
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
	if target == host.ServerRef {
		return true, nil
	}
	// a leader cut off from the others must not take the address from the
	// host the new leader routes it to
	if err := cluster.VerifyLeader(ctx); err != nil {
		return true, err
	}
	logger.Info("routing the Floating IP to the ingress host", "host", host.Name, "server", host.ServerRef, "was", target)
	return true, client.AssignFloatingIP(ctx, ip, host.ServerRef)
}

// confirmServerRef checks a host's server ref with the hoster and records
// it as checked. The first host's ref came from its metadata service
// unchecked; a joined host's was checked as it joined. A ref the hoster
// does not confirm keeps the Floating IP off the host, and the loop's
// condition (ingressNotHeld) raises the alert.
func confirmServerRef(ctx context.Context, client hoster.Hoster, propose Propose, host *state.Host,
	logger *slog.Logger,
) error {
	if err := hoster.CheckServerRef(ctx, client, host.ServerRef, host.Underlay, host.PublicAddress); err != nil {
		return fmt.Errorf("host %s: %w", host.Name, err)
	}
	confirm := state.ConfirmServerRef{Host: host.ID, ServerRef: host.ServerRef}
	if _, err := commit(ctx, propose, state.KindConfirmServerRef, confirm); err != nil {
		return fmt.Errorf("record the checked server ref of host %s: %w", host.Name, err)
	}
	logger.Info("the hoster confirmed the host's server", "host", host.Name, "server", host.ServerRef)
	return nil
}

// OpenHoster makes the client of the hoster cluster.yaml names, with its
// token opened; the Floating IP and the join use it.
func OpenHoster(current *state.State, open Open, connect ConnectHoster) (hoster.Hoster, error) {
	config := current.Config.Spec
	sealed, ok := current.ClusterSecret(config.Hoster.Token)
	if !ok {
		return nil, fmt.Errorf("the hoster's token %s is not among the cluster's secrets", config.Hoster.Token)
	}
	token, err := open(sealed)
	if err != nil {
		return nil, fmt.Errorf("open the hoster's token: %w", err)
	}
	return connect(config.Hoster, string(token))
}

// ingressHost is the host that should hold the Floating IP: the recorded
// one while it can (no switching back), else the first of
// ingress.preferred_hosts that can, else the one with the lowest ID that
// can. A host can when it is active and the hoster checked its server ref,
// the hoster's ID of its server. Only while no such host is there does a
// host with a ref not checked yet count (the first host, before its check
// in holdFloatingIP): so a ref the hoster refuses keeps the Floating IP
// from no host that has a checked one.
func ingressHost(current *state.State) (*state.Host, bool) {
	if host, ok := chooseIngress(current, true); ok {
		return host, true
	}
	return chooseIngress(current, false)
}

// chooseIngress is ingressHost's choice among the hosts with a server ref,
// and with checked ones only if checkedOnly says so.
func chooseIngress(current *state.State, checkedOnly bool) (*state.Host, bool) {
	canHold := func(host *state.Host) bool {
		return host != nil && host.Phase == state.Active && host.ServerRef != "" &&
			(host.ServerRefChecked || !checkedOnly)
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
