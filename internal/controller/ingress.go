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

	"github.com/webcodingcat/clowder/internal/clusterdir"
	"github.com/webcodingcat/clowder/internal/hoster"
	"github.com/webcodingcat/clowder/internal/state"
)

// Open unseals a secret of the state (datakey.Key.Open).
type Open func(sealed state.Sealed) ([]byte, error)

// ConnectHoster makes the client of the hoster cluster.yaml names, with
// its token in clear (hoster.New).
type ConnectHoster func(config clusterdir.Hoster, token string) (hoster.Hoster, error)

// hosterTimeout bounds one look at the hoster, an assignment included.
const hosterTimeout = 2 * time.Minute

// RunIngress keeps the Floating IP on the ingress host while this agent
// leads, until ctx ends: at a change of the state and every interval, but
// it reads the hoster's API at most once per interval (Hetzner allows 3600
// requests an hour; this takes about 120). While it fails, it holds the
// condition ingressNotHeld, which the alerts turn into an alert.
func RunIngress(ctx context.Context, cluster Cluster, propose Propose, open Open, connect ConnectHoster,
	conditions *Conditions, logger *slog.Logger,
) {
	var next time.Time // when the API may be read again
	for {
		changed := cluster.Changed()
		if cluster.IsLeader() && !time.Now().Before(next) {
			asked, err := holdIngress(ctx, cluster, propose, open, connect, logger)
			if asked {
				next = time.Now().Add(interval)
			}
			if err != nil {
				logger.Warn("the Floating IP is not on the ingress host", "error", err)
			}
			// a pass that did not ask the hoster proves nothing, unless it
			// found no host to hold the Floating IP
			if asked || err != nil {
				conditions.Hold(ingressNotHeld, err != nil)
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

// holdIngress records the ingress host when it changes, then routes the
// Floating IP to it if the hoster has it elsewhere (moved by hand, or the
// host is new). It tells whether it asked the hoster's API, which counts
// against the API's limit.
func holdIngress(ctx context.Context, cluster Cluster, propose Propose, open Open, connect ConnectHoster,
	logger *slog.Logger,
) (asked bool, err error) {
	var copied *state.State
	cluster.Read(func(current *state.State) { copied, err = current.Copy() })
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
	if copied.Ingress == nil || *copied.Ingress != host.ID {
		logger.Info("the ingress moves", "host", host.Name)
		if err := commit(ctx, propose, state.KindSetIngress, state.SetIngress{Host: host.ID}); err != nil {
			return false, fmt.Errorf("record the ingress host: %w", err)
		}
	}

	ip, err := netip.ParseAddr(config.Ingress.FloatingIP)
	if err != nil {
		return false, fmt.Errorf("read the Floating IP: %w", err)
	}
	sealed, ok := copied.Secrets[config.Hoster.Token.Secret]
	if !ok {
		return false, fmt.Errorf("the hoster's token %s is not among the cluster's secrets", config.Hoster.Token.Secret)
	}
	token, err := open(sealed)
	if err != nil {
		return false, fmt.Errorf("open the hoster's token: %w", err)
	}
	client, err := connect(config.Hoster, string(token))
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(ctx, hosterTimeout)
	defer cancel()
	target, err := client.FloatingIPTarget(ctx, ip)
	if err != nil {
		return true, err
	}
	if target == host.ServerRef {
		return true, nil
	}
	logger.Info("routing the Floating IP to the ingress host", "host", host.Name, "server", host.ServerRef, "was", target)
	return true, client.AssignFloatingIP(ctx, ip, host.ServerRef)
}

// ingressHost is the host that should hold the Floating IP: the recorded
// one while it can (no switching back), else the first of
// ingress.preferred_hosts that can, else the one with the lowest ID that
// can. A host can when it is active and has a server ref, the hoster's ID
// of its server.
func ingressHost(current *state.State) (*state.Host, bool) {
	canHold := func(host *state.Host) bool {
		return host != nil && host.Phase == state.Active && host.ServerRef != ""
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
