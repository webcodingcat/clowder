package controller

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/webcodingcat/clowder/internal/clusterspec"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/notify"
	"github.com/webcodingcat/clowder/internal/state"
)

const (
	// alertsInterval is how often the alerts loop looks while the state
	// does not change. The heartbeat's loop looks this often for a
	// heartbeat in a new configuration.
	alertsInterval = 15 * time.Second
	// remindEvery is how often an open alert is sent again.
	remindEvery = 6 * time.Hour
	// firstRetry and lastRetry bound the wait before a failed delivery is
	// tried again; it doubles from the first to the last.
	firstRetry = time.Minute
	lastRetry  = time.Hour
	// maxOutbox is the most deliveries that may wait. When the outbox is
	// full, the oldest are dropped first.
	maxOutbox = 200
)

// Connect makes the notifier of an alert channel (notify.New).
type Connect func(channel clusterspec.AlertChannel, secrets notify.Secrets) (notify.Notifier, error)

// Alerts raises and resolves the cluster's alerts while this agent leads,
// and tells the channels. It sends a message when an alert is raised or
// gets worse, every remindEvery while it is open, and when it is resolved.
// Its second loop calls the heartbeat. The alerts are in the state, but the
// deliveries are only in this agent's memory. So after a change of leader,
// the new leader sends one reminder of each open alert: a duplicate is
// better than a lost alert.
//
// One pass moves a message through these steps:
//   - wantedAlerts says which alerts should be open.
//   - put-alert and resolve-alert make the state match.
//   - notice compares the open alerts with what the channels were told,
//     and puts the messages that are due into the outbox.
//   - deliver sends them and keeps the failed ones for another try.
type Alerts struct {
	cluster Cluster
	propose Propose
	open    Open
	// connect is notify.New; tests replace it
	connect    Connect
	conditions *Conditions
	logger     *slog.Logger

	// mu guards outbox, which the API reads (Failures); the other fields
	// belong to the loop alone
	mu     sync.Mutex
	outbox []delivery
	// noticed are the open alerts that the channels were told of, and when
	noticed map[string]notice
	// raisedHere are the alerts that this leader raised. Their first
	// message is "raised", not the reminder that a new leader sends for an
	// alert it found already open.
	raisedHere map[string]bool
}

type notice struct {
	alert state.Alert
	// peak is the highest severity that the channels were told of. The
	// all-clear is sent with this severity, so a channel that takes only
	// critical messages gets the all-clear too.
	peak string
	at   time.Time
}

// delivery is a message that waits for its channel.
type delivery struct {
	channel int // index in alerts.channels
	// generation is the generation of the configuration that the message
	// was meant for. In a newer configuration the index may name another
	// channel, so the message is then dropped.
	generation int64
	message    notify.Message
	attempts   int
	next       time.Time
	lastErr    string
}

// NewAlerts makes the alerts loop. conditions are the conditions that the
// other loops hold.
func NewAlerts(cluster Cluster, propose Propose, open Open, conditions *Conditions, logger *slog.Logger) *Alerts {
	return &Alerts{
		cluster: cluster, propose: propose, open: open, connect: notify.New, conditions: conditions, logger: logger,
		noticed: map[string]notice{}, raisedHere: map[string]bool{},
	}
}

// Run raises, resolves and delivers until ctx ends.
func (a *Alerts) Run(ctx context.Context) {
	for {
		changed := a.cluster.Changed()
		if a.cluster.IsLeader() {
			a.pass(ctx)
		} else {
			a.forget()
		}
		select {
		case <-ctx.Done():
			return
		case <-changed:
		case <-time.After(alertsInterval):
		}
	}
}

// forget drops what this agent kept while it led, because the new leader
// tells the channels now. It also forgets the conditions that the other
// loops hold, since this is the one place that sees the lead end. What this
// agent saw is stale by the time it might lead again.
func (a *Alerts) forget() {
	a.mu.Lock()
	a.outbox = nil
	a.mu.Unlock()
	clear(a.noticed)
	clear(a.raisedHere)
	a.conditions.Reset()
}

// RunHeartbeat calls the heartbeat's URL every alerts.heartbeat.interval
// while this agent leads, until ctx ends. It is a loop of its own, so a
// channel that is slow to take a message cannot delay the heartbeat and
// make an outside service raise a false alarm.
func (a *Alerts) RunHeartbeat(ctx context.Context) {
	for {
		wait := alertsInterval
		if a.cluster.IsLeader() {
			if every, ok := a.heartbeat(ctx); ok {
				wait = every
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// heartbeat calls the heartbeat's URL once and tells how long to wait until
// the next call. It returns false if the configuration has no heartbeat.
func (a *Alerts) heartbeat(ctx context.Context) (time.Duration, bool) {
	copied, err := a.cluster.SharedCopy()
	if err != nil {
		a.logger.Error("call the heartbeat", "error", err)
		return 0, false
	}
	alerts := copied.Config.Spec.Alerts
	if alerts == nil || alerts.Heartbeat == nil {
		return 0, false
	}
	target, err := a.secrets(copied)(alerts.Heartbeat.URLSecretRef)
	if err == nil {
		err = notify.Heartbeat(ctx, target)
	}
	if err != nil {
		a.logger.Warn("call the heartbeat", "error", err)
	}
	return alerts.Heartbeat.Every(), true
}

// pass brings the open alerts in line with what the cluster shows, then
// tells the channels.
func (a *Alerts) pass(ctx context.Context) {
	copied, err := a.cluster.SharedCopy()
	if err != nil {
		a.logger.Error("raise the alerts", "error", err)
		return
	}
	if !copied.Configured() {
		return // no configuration yet: no channels
	}
	now := time.Now()
	a.conditions.Keep(conditionKeys(copied))
	for id, host := range copied.Hosts {
		if host.Phase != state.Removed {
			a.conditions.Observe(dockerDownKey(id), host.DockerDown)
		}
	}
	for id, stack := range copied.Stacks {
		a.conditions.Observe(stackFailingKey(id), stack.Status == state.StackFailing)
	}
	noQuorum := map[string]bool{}
	for _, host := range copied.Hosts {
		for _, volume := range host.NoQuorum {
			noQuorum[volume] = true
		}
	}
	for id := range copied.Volumes {
		a.conditions.Observe(noQuorumKey(id), noQuorum[id])
	}
	wanted := wantedAlerts(copied, a.conditions, now)
	for _, key := range slices.Sorted(maps.Keys(copied.Alerts)) {
		if _, ok := wanted[key]; !ok {
			a.commitOrLog(ctx, state.KindResolveAlert, state.ResolveAlert{Key: key})
		}
	}
	for _, key := range slices.Sorted(maps.Keys(wanted)) {
		open, alert := copied.Alerts[key], wanted[key]
		if open != nil && open.Severity == alert.Severity && open.Message == alert.Message && open.Subject == alert.Subject {
			continue
		}
		if open == nil {
			a.logger.Warn("alert", "key", key, "severity", alert.Severity, "subject", alert.Subject)
		}
		if a.commitOrLog(ctx, state.KindPutAlert, alert) && open == nil {
			a.raisedHere[key] = true
		}
	}

	var openAlerts map[string]state.Alert
	a.cluster.Read(func(current *state.State) {
		openAlerts = map[string]state.Alert{}
		for key, alert := range current.Alerts {
			openAlerts[key] = *alert
		}
	})
	a.notice(copied.ClusterName, copied.Config.Spec, copied.Config.Generation, openAlerts, now)
	a.deliver(ctx, copied, now)
}

// commitOrLog writes one command and tells whether it committed; a failure
// is logged, and the next pass tries again.
func (a *Alerts) commitOrLog(ctx context.Context, kind string, payload any) bool {
	if _, err := commit(ctx, a.propose, kind, payload); err != nil {
		a.logger.Error("raise the alerts", "command", kind, "error", err)
		return false
	}
	return true
}

// notice puts the messages that the open alerts call for into the outbox:
// an alert that was raised or is worse than the channels were told, a
// reminder, or an alert that was resolved. An alert that gets better (or
// worse again, up to its peak) sends nothing until its reminder or its
// all-clear.
func (a *Alerts) notice(clusterName string, config clusterspec.Config, generation int64, open map[string]state.Alert,
	now time.Time,
) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, key := range slices.Sorted(maps.Keys(open)) {
		alert := open[key]
		last, known := a.noticed[key]
		var event string
		switch {
		case !known && a.raisedHere[key]:
			event = notify.Raised
		case !known:
			event = notify.Reminder // open before this agent led
		case alert.Severity != last.peak && clusterspec.AtLeast(alert.Severity, last.peak):
			event = notify.Raised // worse
		case now.Sub(last.at) >= remindEvery:
			event = notify.Reminder
		default:
			last.alert = alert
			a.noticed[key] = last
			continue
		}
		delete(a.raisedHere, key)
		peak := alert.Severity
		if known && clusterspec.AtLeast(last.peak, peak) {
			peak = last.peak
		}
		a.noticed[key] = notice{alert: alert, peak: peak, at: now}
		a.enqueue(clusterName, config, generation, alert, event, alert.Severity, now)
	}
	for _, key := range slices.Sorted(maps.Keys(a.noticed)) {
		if _, still := open[key]; !still {
			last := a.noticed[key]
			a.enqueue(clusterName, config, generation, last.alert, notify.Resolved, last.peak, now)
			delete(a.noticed, key)
		}
	}
}

// enqueue adds a message to the outbox for each channel that takes its
// severity. When the outbox is full, the oldest messages are dropped.
func (a *Alerts) enqueue(clusterName string, config clusterspec.Config, generation int64, alert state.Alert,
	event, severity string, now time.Time,
) {
	if config.Alerts == nil {
		return
	}
	message := notify.Message{
		Cluster: clusterName, Event: event, Key: alert.Key, Kind: alert.Kind, Severity: alert.Severity,
		Subject: alert.Subject, Text: alert.Message, Since: alert.Since,
	}
	for index, channel := range config.Alerts.Channels {
		if clusterspec.AtLeast(severity, channel.MinSeverity) {
			a.outbox = append(a.outbox, delivery{channel: index, generation: generation, message: message, next: now})
		}
	}
	if extra := len(a.outbox) - maxOutbox; extra > 0 {
		a.logger.Error("dropping undelivered alert messages", "count", extra)
		a.outbox = slices.Delete(a.outbox, 0, extra)
	}
}

// deliver sends the messages in the outbox that are due. After each
// failure, a message waits longer. A channel that fails gets nothing more
// in this pass, so a dead mail server cannot hold up the loop. A message
// meant for an earlier configuration is dropped, because its index may
// name another channel now.
func (a *Alerts) deliver(ctx context.Context, copied *state.State, now time.Time) {
	a.mu.Lock()
	due := slices.Clone(a.outbox)
	a.mu.Unlock()
	notifiers := map[int]notify.Notifier{}
	failing := map[int]bool{}
	var kept []delivery
	for _, item := range due {
		if item.generation != copied.Config.Generation {
			a.logger.Warn("dropping an alert message: the configuration changed", "key", item.message.Key,
				"event", item.message.Event)
			continue
		}
		if now.Before(item.next) || failing[item.channel] {
			kept = append(kept, item)
			continue
		}
		err := a.send(ctx, copied, notifiers, item)
		switch {
		case err == nil:
			continue
		case ctx.Err() != nil:
			kept = append(kept, item) // stopping: the channel did not fail
			continue
		}
		failing[item.channel] = true
		item.attempts++
		item.lastErr = err.Error()
		// cap the shift: a channel that is down for a day must not overflow it
		item.next = now.Add(min(firstRetry<<min(item.attempts-1, 10), lastRetry))
		a.logger.Warn("deliver an alert", "channel", channelName(copied.Config.Spec, item.channel),
			"key", item.message.Key, "event", item.message.Event, "error", err, "retry_in", item.next.Sub(now))
		kept = append(kept, item)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.outbox = kept // only this loop adds to it
}

// send delivers one message, making the channel's notifier once per pass.
func (a *Alerts) send(ctx context.Context, copied *state.State, notifiers map[int]notify.Notifier, item delivery) error {
	config := copied.Config.Spec
	if config.Alerts == nil || item.channel >= len(config.Alerts.Channels) {
		// deliver has already dropped the messages of an earlier configuration
		return fmt.Errorf("the configuration has no alerts.channels[%d]", item.channel)
	}
	notifier, ok := notifiers[item.channel]
	if !ok {
		var err error
		notifier, err = a.connect(config.Alerts.Channels[item.channel], a.secrets(copied))
		if err != nil {
			return err
		}
		notifiers[item.channel] = notifier
	}
	return notifier.Send(ctx, item.message)
}

// secrets opens the values the configuration names from the state.
func (a *Alerts) secrets(copied *state.State) notify.Secrets {
	return func(ref names.SecretKeyRef) (string, error) {
		encrypted, ok := copied.SystemSecret(ref)
		if !ok {
			return "", fmt.Errorf("%s has no Secret %s with the key %s", names.System, ref.Name, ref.Key)
		}
		opened, err := a.open(encrypted)
		return string(opened), err
	}
}

// channelName names a channel of the configuration for people.
func channelName(config clusterspec.Config, index int) string {
	if config.Alerts == nil || index >= len(config.Alerts.Channels) {
		return fmt.Sprintf("alerts.channels[%d]", index)
	}
	return fmt.Sprintf("alerts.channels[%d] (%s)", index, config.Alerts.Channels[index].Type)
}

// Failure is a message a channel has not taken yet.
type Failure struct {
	Channel   string
	Key       string
	Event     string
	Attempts  int
	LastError string
}

// Failures are the messages that failed at least once and wait for
// another try. They are empty on an agent that does not lead.
func (a *Alerts) Failures() []Failure {
	var config clusterspec.Config
	a.cluster.Read(func(current *state.State) { config = current.Config.Spec })
	a.mu.Lock()
	defer a.mu.Unlock()
	failures := []Failure{}
	for _, item := range a.outbox {
		if item.attempts > 0 {
			failures = append(failures, Failure{
				Channel: channelName(config, item.channel), Key: item.message.Key, Event: item.message.Event,
				Attempts: item.attempts, LastError: item.lastErr,
			})
		}
	}
	return failures
}

// TestResult is how a channel took the test alert; Err is nil if it did.
type TestResult struct {
	Channel string
	Err     error
}

// Test sends a test alert to every channel, whatever its minSeverity. It
// sends at once and from this agent, and tells how each channel took it.
// It returns no results when the configuration names no channel.
func (a *Alerts) Test(ctx context.Context) ([]TestResult, error) {
	copied, err := a.cluster.SharedCopy()
	if err != nil {
		return nil, err
	}
	config := copied.Config.Spec
	if config.Alerts == nil {
		return nil, nil
	}
	message := notify.Message{
		Cluster: copied.ClusterName, Event: notify.Test, Key: "test", Kind: "test", Severity: clusterspec.SeverityInfo,
		Subject: "test alert", Text: "clowder alert test: this channel gets the cluster's alerts.", Since: time.Now(),
	}
	var results []TestResult
	for index := range config.Alerts.Channels {
		err := a.send(ctx, copied, map[int]notify.Notifier{}, delivery{channel: index, message: message})
		results = append(results, TestResult{Channel: channelName(config, index), Err: err})
	}
	return results, nil
}
