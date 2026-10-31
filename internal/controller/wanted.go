package controller

import (
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/webcodingcat/clowder/internal/clusterspec"
	"github.com/webcodingcat/clowder/internal/state"
)

// How long a condition the state does not show must last before it raises
// an alert: a short hiccup is no alert.
const (
	dockerDownFor  = 2 * time.Minute
	ingressLostFor = 5 * time.Minute
	stackWaitFor   = 5 * time.Minute
)

// The keys of the conditions other loops hold (Conditions). They name a
// host by its ID, which never changes; the alerts' keys name it by its
// name, which people read.
const ingressNotHeld = "ingress-not-held"

// overCapacityKey is the condition of a host over capacity.
func overCapacityKey(host state.HostID) string {
	return fmt.Sprintf("over-capacity/%d", host)
}

// dockerDownKey is the condition of a host whose Docker does not answer.
func dockerDownKey(host state.HostID) string {
	return fmt.Sprintf("docker-down/%d", host)
}

// level is a threshold of a value in percent: it raises at raise and clears
// only below clear, so a value that hovers at the threshold does not raise
// and clear at every report.
type level struct {
	severity     string
	raise, clear int
}

// The thresholds of the pools (data and metadata) and the root file
// system, highest first.
var (
	poolCritical = level{clusterspec.SeverityCritical, 95, 90}
	poolWarning  = level{clusterspec.SeverityWarning, 80, 75}
	poolLevels   = []level{poolCritical, poolWarning}
	rootLevels   = []level{{clusterspec.SeverityWarning, 90, 85}}
)

// leveled is the severity a value has against levels, given the alert that
// is open for it; false: no alert.
func leveled(value int, open *state.Alert, levels []level) (string, bool) {
	for _, threshold := range levels {
		stays := open != nil && clusterspec.AtLeast(open.Severity, threshold.severity) && value >= threshold.clear
		if value >= threshold.raise || stays {
			return threshold.severity, true
		}
	}
	return "", false
}

// wantedAlerts are the alerts the state and the conditions call for now,
// by key. Subjects and messages hold no value that changes from report to
// report: each change is a Raft write.
func wantedAlerts(current *state.State, conditions *Conditions, now time.Time) map[string]state.PutAlert {
	wanted := map[string]state.PutAlert{}
	add := func(key, kind, severity, subject, message string) {
		wanted[key] = state.PutAlert{Key: key, Kind: kind, Severity: severity, Subject: subject, Message: message}
	}
	// a condition raises its alert once it lasted; an alert that is open
	// stays while its condition holds, and while no loop has looked at it
	// yet (a new leader counts again from zero)
	held := func(condition, alert string, atLeast time.Duration) bool {
		if current.Alerts[alert] != nil && !conditions.Seen(condition) {
			return true
		}
		since, ok := conditions.Since(condition)
		return ok && (now.Sub(since) >= atLeast || current.Alerts[alert] != nil)
	}
	for _, id := range slices.Sorted(maps.Keys(current.Hosts)) {
		host := current.Hosts[id]
		if host.Phase == state.Removed {
			continue
		}
		for _, name := range slices.Sorted(maps.Keys(host.Pools)) {
			pool := host.Pools[name]
			for part, value := range map[string]int{"data": pool.DataPercent, "metadata": pool.MetadataPercent} {
				key := fmt.Sprintf("pool-use/%s/%s/%s", host.Name, name, part)
				if severity, ok := leveled(value, current.Alerts[key], poolLevels); ok {
					add(key, "pool-use", severity, fmt.Sprintf("pool %s on %s: %s filling up", name, host.Name, part),
						fmt.Sprintf("The pool's %s is filling up: a warning from %d %%, critical from %d %%. A thin "+
							"pool that runs full stops every volume in it.", part, poolWarning.raise, poolCritical.raise))
				}
			}
		}
		rootKey := "root-use/" + host.Name
		if severity, ok := leveled(host.RootUsedPercent, current.Alerts[rootKey], rootLevels); ok {
			add(rootKey, "root-use", severity, fmt.Sprintf("root file system of %s filling up", host.Name),
				fmt.Sprintf("The root file system is %d %% full or more: Docker's images and logs live there.",
					rootLevels[0].raise))
		}
		if held(dockerDownKey(id), "docker-down/"+host.Name, dockerDownFor) {
			add("docker-down/"+host.Name, "docker-down", clusterspec.SeverityCritical, fmt.Sprintf("Docker on %s does not answer", host.Name),
				fmt.Sprintf("Docker has not answered the agent's ping for %v or more; the host's stacks may be down.",
					dockerDownFor))
		}
		if held(overCapacityKey(id), "over-capacity/"+host.Name, 0) {
			add("over-capacity/"+host.Name, "over-capacity", clusterspec.SeverityWarning, fmt.Sprintf("%s is over capacity", host.Name),
				"The services at the host's highest priority reserve more than it offers; nothing was pushed out, "+
					"since that would not help. Lower system_reserve or the reservations, or give the host more memory.")
		}
	}
	if held(ingressNotHeld, ingressNotHeld, ingressLostFor) {
		add(ingressNotHeld, ingressNotHeld, clusterspec.SeverityCritical, "the Floating IP is not on the ingress host",
			fmt.Sprintf("For %v or more the leader could not route the Floating IP to a host that can hold it: "+
				"no request reaches the stacks. The agents' logs say why.", ingressLostFor))
	}
	addCertificateAlerts(current, now, add)
	for _, name := range slices.Sorted(maps.Keys(current.Stacks)) {
		stack := current.Stacks[name]
		if stack.WaitReason != "" && now.Sub(stack.WaitingSince) >= stackWaitFor {
			add("stack-waiting/"+name, "stack-waiting", clusterspec.SeverityWarning, fmt.Sprintf("stack %s waits for a place", name),
				"The stack finds no place: "+stack.WaitReason)
		}
	}
	return wanted
}

// addCertificateAlerts adds an alert for each certificate that is not
// renewed while less than half of its renewal window is left, and one if
// acme.renew_before is ignored for any certificate.
func addCertificateAlerts(current *state.State, now time.Time, add func(key, kind, severity, subject, message string)) {
	config := current.Config.Spec.ACME
	var ignoredFor []string
	for _, domain := range slices.Sorted(maps.Keys(current.Certificates)) {
		certificate := current.Certificates[domain]
		before, ignored := renewBefore(certificate, config)
		if ignored {
			ignoredFor = append(ignoredFor, domain)
		}
		if !now.Before(certificate.NotAfter.Add(-before / 2)) {
			add("certificate-not-renewed/"+domain, "certificate-not-renewed", clusterspec.SeverityWarning,
				fmt.Sprintf("the certificate of %s is not renewed", domain),
				fmt.Sprintf("It ends %s and less than half of its renewal window is left. The leader's log says why "+
					"the orders fail; the domain must point at the Floating IP.", certificate.NotAfter.UTC().Format(time.DateOnly)))
		}
	}
	if len(ignoredFor) > 0 {
		add("renew-before-ignored", "renew-before-ignored", clusterspec.SeverityWarning,
			"acme.renew_before is not shorter than a certificate's lifetime",
			fmt.Sprintf("It would renew every new certificate at once, so the leader renews %v when a third of the "+
				"lifetime is left. Set renew_before shorter.", ignoredFor))
	}
}
