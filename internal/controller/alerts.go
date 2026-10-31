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
	"github.com/webcodingcat/clowder/internal/notify"
	"github.com/webcodingcat/clowder/internal/state"
)

const (
	// alertsInterval is how often the alerts loop looks without a change
	// of the state, and the heartbeat's loop for a heartbeat in a new
	// cluster.yaml.
	alertsInterval = 15 * time.Second
	// remindEvery is how often an open alert is sent again.
	remindEvery = 6 * time.Hour
	// firstRetry and lastRetry bound the wait before a failed delivery is
	// tried again; it doubles from the first to the last.
	firstRetry = time.Minute
	lastRetry  = time.Hour
	// maxOutbox is how many deliveries wait at most; the oldest go first.
	maxOutbox = 200
)

// Connect makes the notifier of an alert channel (notify.New).
type Connect func(channel clusterspec.AlertChannel, secrets notify.Secrets, files map[string][]byte) (notify.Notifier, error)

// Alerts raises and resolves the cluster's alerts while this agent leads,
// and tells the channels: when an alert is raised or gets worse, every
// remindEvery while it is open, and when it is resolved. Its second loop
// calls the heartbeat. The alerts are in the state; the deliveries are in this
// agent's memory, so after a change of leader the new leader reminds of
// each open alert once (a duplicate rather than a lost alert).
//
// A message flows through one pass: wantedAlerts says what should be open,
// put-alert and resolve-alert make the state say so, notice compares the
// open alerts with what the channels were told and puts the messages that
// are due into the outbox, and deliver sends them, keeping the failed ones
// for another try.
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
	// noticed are the open alerts the channels were told of, and when
	noticed map[string]notice
	// raisedHere are the alerts this leader raised: their first message is
	// "raised", not the reminder a new leader sends of an alert it found
	raisedHere map[string]bool
}

type notice struct {
	alert state.Alert
	// peak is the highest severity the channels were told of: the
	// all-clear goes with it, so a channel that took only the critical
	// message gets the all-clear too
	peak string
	at   time.Time
}

// delivery is a message that waits for its channel.
type delivery struct {
	channel int // index in alerts.channels
	// revision is the cluster.yaml it was meant for: after a new one the
	// index may name another channel, so the message is dropped
	revision int64
	message  notify.Message
	attempts int
	next     time.Time
	lastErr  string
}

// NewAlerts is the alerts loop, with the conditions the other loops hold.
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

// forget drops what this agent kept while it led: the new leader tells the
// channels. It also forgets the conditions the other loops hold, as the one
// place that sees the lead end; what this agent saw is stale by the time it
// might lead again.
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
// channel that is slow to take a message cannot make it late, and an
// outside service alarm wrongly.
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

// heartbeat calls the heartbeat's URL once and tells how long until the
// next call; false without a heartbeat in cluster.yaml.
func (a *Alerts) heartbeat(ctx context.Context) (time.Duration, bool) {
	var copied *state.State
	var err error
	a.cluster.Read(func(current *state.State) { copied, err = current.Copy() })
	if err != nil {
		a.logger.Error("call the heartbeat", "error", err)
		return 0, false
	}
	alerts := copied.Config.Spec.Alerts
	if alerts == nil || alerts.Heartbeat == nil {
		return 0, false
	}
	target, err := a.secrets(copied)(alerts.Heartbeat.URL.Secret)
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
	var copied *state.State
	var err error
	a.cluster.Read(func(current *state.State) { copied, err = current.Copy() })
	if err != nil {
		a.logger.Error("raise the alerts", "error", err)
		return
	}
	if copied.Config.Revision == 0 {
		return // no cluster.yaml yet: no channels
	}
	now := time.Now()
	for id, host := range copied.Hosts {
		a.conditions.Hold(dockerDownKey(id), host.DockerDown)
	}
	wanted := wantedAlerts(copied, a.conditions, now)
	for _, key := range slices.Sorted(maps.Keys(copied.Alerts)) {
		if _, ok := wanted[key]; !ok {
			a.commit(ctx, state.KindResolveAlert, state.ResolveAlert{Key: key})
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
		if a.commit(ctx, state.KindPutAlert, alert) && open == nil {
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
	a.notice(copied.Config.Spec, copied.Config.Revision, openAlerts, now)
	a.deliver(ctx, copied, now)
}

// commit writes one command and tells whether it committed; a failure is
// logged, and the next pass tries again.
func (a *Alerts) commit(ctx context.Context, kind string, payload any) bool {
	if err := commit(ctx, a.propose, kind, payload); err != nil {
		a.logger.Error("raise the alerts", "command", kind, "error", err)
		return false
	}
	return true
}

// notice puts the messages the open alerts call for into the outbox: one
// raised or got worse than the channels were told, a reminder, one
// resolved. An alert that gets better (and worse again up to its peak)
// sends nothing until its reminder or its all-clear.
func (a *Alerts) notice(config clusterspec.Config, revision int64, open map[string]state.Alert, now time.Time) {
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
		a.enqueue(config, revision, alert, event, alert.Severity, now)
	}
	for _, key := range slices.Sorted(maps.Keys(a.noticed)) {
		if _, still := open[key]; !still {
			last := a.noticed[key]
			a.enqueue(config, revision, last.alert, notify.Resolved, last.peak, now)
			delete(a.noticed, key)
		}
	}
}

// enqueue adds a message to the outbox of each channel that takes the
// severity; the oldest go when it is full.
func (a *Alerts) enqueue(config clusterspec.Config, revision int64, alert state.Alert, event, severity string, now time.Time) {
	if config.Alerts == nil {
		return
	}
	message := notify.Message{
		Cluster: config.Name, Event: event, Key: alert.Key, Kind: alert.Kind, Severity: alert.Severity,
		Subject: alert.Subject, Text: alert.Message, Since: alert.Since,
	}
	for index, channel := range config.Alerts.Channels {
		if clusterspec.AtLeast(severity, channel.MinSeverity) {
			a.outbox = append(a.outbox, delivery{channel: index, revision: revision, message: message, next: now})
		}
	}
	if extra := len(a.outbox) - maxOutbox; extra > 0 {
		a.logger.Error("dropping undelivered alert messages", "count", extra)
		a.outbox = slices.Delete(a.outbox, 0, extra)
	}
}

// deliver sends what is due in the outbox; a failure waits longer each
// time. A channel that fails gets nothing more in this pass, so a dead mail
// server cannot hold up the loop. A message meant for an earlier
// cluster.yaml is dropped: the index may name another channel now.
func (a *Alerts) deliver(ctx context.Context, copied *state.State, now time.Time) {
	a.mu.Lock()
	due := slices.Clone(a.outbox)
	a.mu.Unlock()
	notifiers := map[int]notify.Notifier{}
	failing := map[int]bool{}
	var kept []delivery
	for _, item := range due {
		if item.revision != copied.Config.Revision {
			a.logger.Warn("dropping an alert message: cluster.yaml changed", "key", item.message.Key,
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
			kept = append(kept, item) // stopping: no failure of the channel
			continue
		}
		failing[item.channel] = true
		item.attempts++
		item.lastErr = err.Error()
		// the shift stays small: a channel down for a day must not overflow
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
		// deliver drops messages of an earlier cluster.yaml first
		return fmt.Errorf("cluster.yaml has no alerts.channels[%d]", item.channel)
	}
	notifier, ok := notifiers[item.channel]
	if !ok {
		var err error
		notifier, err = a.connect(config.Alerts.Channels[item.channel], a.secrets(copied), copied.Config.Files)
		if err != nil {
			return err
		}
		notifiers[item.channel] = notifier
	}
	return notifier.Send(ctx, item.message)
}

// secrets opens the secrets of cluster.yaml from the state.
func (a *Alerts) secrets(copied *state.State) notify.Secrets {
	return func(name string) (string, error) {
		sealed, ok := copied.ClusterSecrets[name]
		if !ok {
			return "", fmt.Errorf("secret %s is not among the cluster's secrets", name)
		}
		opened, err := a.open(sealed)
		return string(opened), err
	}
}

// channelName names a channel of cluster.yaml for people.
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
// another try; empty on an agent that does not lead.
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

// Test sends a test alert to every channel, whatever its min_severity, at
// once and from this agent, and tells how each took it.
func (a *Alerts) Test(ctx context.Context) ([]TestResult, error) {
	var copied *state.State
	var err error
	a.cluster.Read(func(current *state.State) { copied, err = current.Copy() })
	if err != nil {
		return nil, err
	}
	config := copied.Config.Spec
	if config.Alerts == nil || len(config.Alerts.Channels) == 0 {
		return nil, fmt.Errorf("cluster.yaml names no alert channel")
	}
	message := notify.Message{
		Cluster: config.Name, Event: notify.Test, Key: "test", Kind: "test", Severity: clusterspec.SeverityInfo,
		Subject: "test alert", Text: "clowder alert test: this channel gets the cluster's alerts.", Since: time.Now(),
	}
	var results []TestResult
	for index := range config.Alerts.Channels {
		err := a.send(ctx, copied, map[int]notify.Notifier{}, delivery{channel: index, message: message})
		results = append(results, TestResult{Channel: channelName(config, index), Err: err})
	}
	return results, nil
}
