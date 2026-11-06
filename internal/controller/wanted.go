package controller

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/webcodingcat/clowder/internal/clusterspec"
	"github.com/webcodingcat/clowder/internal/state"
)

// How long a condition must last before it raises an alert. A short hiccup
// raises none.
const (
	dockerDownFor   = 2 * time.Minute
	ingressLostFor  = 5 * time.Minute
	stackWaitFor    = 5 * time.Minute
	stackFailingFor = 2 * time.Minute
	noQuorumFor     = 2 * time.Minute
)

// A condition's key names a host by its ID, which never changes. An alert's
// key names a host by its name, which people read.
const (
	ingressNotHeld = "ingress-not-held"
	// hetznerTokenRefused is observed only in the calls that fencing and the
	// Floating IP make anyway
	hetznerTokenRefused = "hetzner-token"
)

// conditionKeys must list every condition key, or every alerts pass forgets
// that condition (Conditions.Keep).
func conditionKeys(current *state.State) map[string]bool {
	keys := map[string]bool{ingressNotHeld: true, hetznerTokenRefused: true}
	for id, host := range current.Hosts {
		if host.Phase != state.Removed {
			keys[overCapacityKey(id)] = true
			keys[dockerDownKey(id)] = true
			keys[fencingBlockedKey(id)] = true
		}
	}
	for id := range current.Stacks {
		keys[stackFailingKey(id)] = true
	}
	for id := range current.Volumes {
		keys[noQuorumKey(id)] = true
	}
	return keys
}

func overCapacityKey(host state.HostID) string {
	return fmt.Sprintf("over-capacity/%d", host)
}

func dockerDownKey(host state.HostID) string {
	return fmt.Sprintf("docker-down/%d", host)
}

func fencingBlockedKey(host state.HostID) string {
	return fmt.Sprintf("fencing-blocked/%d", host)
}

// stackFailingKey names the stack by its ID, like the alert's key.
func stackFailingKey(stack string) string {
	return "stack-failing/" + stack
}

// noQuorumKey names the volume by its ID, like the alert's key.
func noQuorumKey(volume string) string {
	return "no-quorum/" + volume
}

// level is a threshold of a value in percent. The alert is raised at raise
// and resolved only below clear. So a value that hovers near the threshold
// does not raise and resolve the alert at every report.
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

// leveled returns the severity that a value has against levels, given the
// alert that is open for it. It returns false when the value calls for no
// alert.
func leveled(value int, open *state.Alert, levels []level) (string, bool) {
	for _, threshold := range levels {
		stays := open != nil && clusterspec.AtLeast(open.Severity, threshold.severity) && value >= threshold.clear
		if value >= threshold.raise || stays {
			return threshold.severity, true
		}
	}
	return "", false
}

// wantedAlerts returns the alerts that the state and the conditions call
// for now, by key. Subjects and messages hold no value that changes from
// report to report, because each change is a Raft write.
func wantedAlerts(current *state.State, conditions *Conditions, now time.Time) map[string]state.PutAlert {
	wanted := map[string]state.PutAlert{}
	add := func(key, kind, severity, subject, message string) {
		wanted[key] = state.PutAlert{Key: key, Kind: kind, Severity: severity, Subject: subject, Message: message}
	}
	// a condition raises its alert once it has lasted long enough. An open
	// alert stays while its condition holds, and while no loop has looked
	// at the condition yet (a new leader starts counting from zero).
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
		addHostAlerts(host, held(fencingBlockedKey(id), "fencing-blocked/"+host.Name, 0), add)
		if held(overCapacityKey(id), "over-capacity/"+host.Name, 0) {
			add("over-capacity/"+host.Name, "over-capacity", clusterspec.SeverityWarning, fmt.Sprintf("%s is over capacity", host.Name),
				"The services at the host's highest priority reserve more than it offers; nothing was pushed out, "+
					"since that would not help. Lower systemReserve or the reservations, or give the host more memory.")
		}
	}
	if held(ingressNotHeld, ingressNotHeld, ingressLostFor) {
		add(ingressNotHeld, ingressNotHeld, clusterspec.SeverityCritical, "the Floating IP is not on the ingress host",
			fmt.Sprintf("For %v or more the leader could not route the Floating IP to a host that can hold it: "+
				"no request reaches the stacks. The agents' logs say why.", ingressLostFor))
	}
	if held(hetznerTokenRefused, hetznerTokenRefused, 0) {
		add(hetznerTokenRefused, hetznerTokenRefused, clusterspec.SeverityCritical, "Hetzner refuses the cluster's token",
			"Hetzner refused the token the ClusterConfiguration names, or its project has not the cluster's "+
				"servers or Floating IP: fencing and the Floating IP stop until a token of the cluster's project "+
				"is applied. The agents' logs say what was refused.")
	}
	if !current.RecoveredAt.IsZero() {
		add("no-ha", "no-ha", clusterspec.SeverityCritical, "the cluster is not highly available",
			fmt.Sprintf("clowderd recover left the cluster with one host on %s. The volumes kept no second "+
				"copy, and a failure of that host stops the cluster. Join two control-plane "+
				"hosts (clowder host join-token --roles); the alert ends once %d control-plane hosts are in service.",
				current.RecoveredAt.Format(time.RFC3339), state.MinHAHosts))
	}
	addVolumeAlerts(current, held, add)
	addCertificateAlerts(current, now, add)
	for _, name := range slices.Sorted(maps.Keys(current.Stacks)) {
		stack := current.Stacks[name]
		switch {
		case stack.WaitReason == "" || now.Sub(stack.WaitingSince) < stackWaitFor:
		case stack.Host != nil && state.FailsOver(current.Hosts[*stack.Host].Phase):
			add("stack-waiting/"+name, "stack-waiting", clusterspec.SeverityWarning, fmt.Sprintf("stack %s is down", name),
				"Its host is down, and the stack finds no other host to run on: "+stack.WaitReason+". A volume with "+
					"replicas 1 has no copy elsewhere: the stack waits for its host, or a restore of the volume.")
		case stack.Host != nil:
			// the stack runs; only its move waits (for a drain or a user's request)
			add("stack-waiting/"+name, "stack-waiting", clusterspec.SeverityWarning, fmt.Sprintf("stack %s cannot move", name),
				"The stack runs where it is, but finds no host to move to: "+stack.WaitReason)
		default:
			add("stack-waiting/"+name, "stack-waiting", clusterspec.SeverityWarning, fmt.Sprintf("stack %s waits for a place", name),
				"The stack finds no place: "+stack.WaitReason)
		}
		if key := stackFailingKey(name); held(key, key, stackFailingFor) {
			add(key, "stack-failing", clusterspec.SeverityCritical, fmt.Sprintf("stack %s does not come up", name),
				fmt.Sprintf("For %v or more its host could not bring the stack up and tries again at every pass: %s. "+
					"The agent's log says more.", stackFailingFor, stack.UpError))
		}
	}
	return wanted
}

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
			"acme.renewBefore is not shorter than a certificate's lifetime",
			fmt.Sprintf("It would renew every new certificate at once, so the leader renews %v when a third of the "+
				"lifetime is left. Set renewBefore shorter.", ignoredFor))
	}
}

func addHostAlerts(host *state.Host, blocked bool, add func(key, kind, severity, subject, message string)) {
	switch host.Phase {
	case state.Failed, state.Fencing:
		add("host-failed/"+host.Name, "host-failed", clusterspec.SeverityWarning, fmt.Sprintf("%s does not answer", host.Name),
			fmt.Sprintf("A majority of the voters has not heard the host for %v or more. If another host can take its stacks, "+
				"the leader powers it off through Hetzner's API, and then they start there; otherwise it stays as it "+
				"is until it answers again.", FailAfter))
	case state.Fenced, state.Recovering:
		add("host-fenced/"+host.Name, "host-fenced", clusterspec.SeverityCritical, fmt.Sprintf("%s was fenced", host.Name),
			"The leader powered the host off after it failed, and its stacks started elsewhere. It is powered on "+
				"again, catches its volumes up (recovering) and takes stacks once it is active.")
	case state.Disabled:
		add("host-disabled/"+host.Name, "host-disabled", clusterspec.SeverityCritical, fmt.Sprintf("%s is disabled", host.Name),
			fmt.Sprintf("The host was fenced %d times within %v and stays off. Find out why, then run clowder host "+
				"enable %s.", state.FlapLimit, state.FlapWindow, host.Name))
	}
	if blocked {
		add("fencing-blocked/"+host.Name, "fencing-blocked", clusterspec.SeverityCritical,
			fmt.Sprintf("%s cannot be fenced", host.Name),
			"The host failed, but the leader cannot power it off: no Hetzner token yet, an unconfirmed server, "+
				"Hetzner's API, or more hosts failed at once. Its stacks do not start elsewhere. The leader's log says why. If "+
				"you know the host is off, run clowder host fence-confirm "+host.Name+".")
	}
}

// addVolumeAlerts raises a split brain at once, because it needs a human and
// does not go away by itself. A primary without DRBD's quorum
// raises an alert only after noQuorumFor, because a failing host takes the
// quorum away until the failover.
func addVolumeAlerts(current *state.State, held func(condition, alert string, atLeast time.Duration) bool,
	add func(key, kind, severity, subject, message string),
) {
	splitBrain, noQuorum := map[string][]string{}, map[string][]string{}
	for _, id := range slices.Sorted(maps.Keys(current.Hosts)) {
		host := current.Hosts[id]
		for _, volume := range host.SplitBrain {
			splitBrain[volume] = append(splitBrain[volume], host.Name)
		}
		for _, volume := range host.NoQuorum {
			noQuorum[volume] = append(noQuorum[volume], host.Name)
		}
	}
	for _, volume := range slices.Sorted(maps.Keys(splitBrain)) {
		add("split-brain/"+volume, "split-brain", clusterspec.SeverityCritical, fmt.Sprintf("volume %s: split brain", volume),
			fmt.Sprintf("Copies of the volume changed apart and DRBD cannot tell which to keep, so they no longer "+
				"connect (a copy on %s stands alone from a peer). Look at the copies, then run clowder volume "+
				"resolve-splitbrain %s --keep <host>: the copies on the other hosts give up their changes since "+
				"the split.", strings.Join(splitBrain[volume], ", "), volume))
	}
	for _, volume := range slices.Sorted(maps.Keys(current.Volumes)) {
		if key := noQuorumKey(volume); held(key, key, noQuorumFor) {
			add(key, "no-quorum", clusterspec.SeverityCritical, fmt.Sprintf("volume %s has no quorum", volume),
				fmt.Sprintf("For %v or more the volume's primary copy (on %s) reaches too few of its peers: DRBD "+
					"suspends its writes, and its stack hangs until enough peers are back.",
					noQuorumFor, strings.Join(noQuorum[volume], ", ")))
		}
	}
}
