package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/webcodingcat/clowder/internal/clusterdir"
	"github.com/webcodingcat/clowder/internal/notify"
	"github.com/webcodingcat/clowder/internal/state"
)

func TestAThresholdRaisesAtItsValueAndClearsOnlyBelowItsClear(t *testing.T) {
	open := func(severity string) *state.Alert { return &state.Alert{Severity: severity} }
	for _, test := range []struct {
		value int
		open  *state.Alert
		want  string // "" for no alert
	}{
		{79, nil, ""},
		{80, nil, clusterdir.SeverityWarning},
		{77, open(clusterdir.SeverityWarning), clusterdir.SeverityWarning}, // hovers at the threshold: stays
		{74, open(clusterdir.SeverityWarning), ""},
		{95, open(clusterdir.SeverityWarning), clusterdir.SeverityCritical},
		{91, open(clusterdir.SeverityCritical), clusterdir.SeverityCritical},
		{89, open(clusterdir.SeverityCritical), clusterdir.SeverityWarning}, // back to warning
		{76, open(clusterdir.SeverityCritical), clusterdir.SeverityWarning},
	} {
		got, _ := leveled(test.value, test.open, poolLevels)
		if got != test.want {
			t.Errorf("%d %% with %+v open: %q, want %q", test.value, test.open, got, test.want)
		}
	}
}

func TestConditionsRaiseAlertsOnlyOnceTheyLasted(t *testing.T) {
	current := &state.State{
		Hosts: map[state.HostID]*state.Host{0: {
			ID: 0, Name: "node0", Phase: state.Active, DockerDown: true, RootUsedPercent: 91,
			Pools: map[string]state.Pool{"vg/thin": {SizeBytes: 1, DataPercent: 96, MetadataPercent: 10}},
		}},
		Stacks: map[string]*state.Stack{
			"default/shop": {
				Namespace: "default", Name: "shop", WaitReason: "no room", WaitingSince: time.Now().Add(-6 * time.Minute),
			},
			"default/blog": {
				Namespace: "default", Name: "blog", WaitReason: "no room", WaitingSince: time.Now().Add(-time.Minute),
			},
		},
	}
	conditions := NewConditions()
	conditions.Hold(dockerDownKey(0), true)
	conditions.Hold(ingressNotHeld, true)
	conditions.Hold(overCapacityKey(0), true)
	keys := func(now time.Time) []string {
		var keys []string
		for key, alert := range wantedAlerts(current, conditions, now) {
			keys = append(keys, key+"="+alert.Severity)
		}
		slices.Sort(keys)
		return keys
	}
	soon := fmt.Sprint(keys(time.Now()))
	want := "[over-capacity/node0=warning pool-use/node0/vg/thin/data=critical root-use/node0=warning stack-waiting/default/shop=warning]"
	if soon != want {
		t.Errorf("at once %s, want %s", soon, want)
	}
	later := keys(time.Now().Add(6 * time.Minute))
	for _, key := range []string{"docker-down/node0=critical", "ingress-not-held=critical", "stack-waiting/default/blog=warning"} {
		if !slices.Contains(later, key) {
			t.Errorf("no %s after six minutes: %v", key, later)
		}
	}
}

func TestACertificateNotRenewedRaisesAnAlert(t *testing.T) {
	now := time.Now()
	current := &state.State{Certificates: map[string]*state.Certificate{
		// 90 days, 30 days' window: alert with less than 15 days left
		"old.example.com":   {Domain: "old.example.com", NotBefore: now.Add(-80 * 24 * time.Hour), NotAfter: now.Add(10 * 24 * time.Hour)},
		"fresh.example.com": {Domain: "fresh.example.com", NotBefore: now.Add(-70 * 24 * time.Hour), NotAfter: now.Add(20 * 24 * time.Hour)},
	}}
	wanted := wantedAlerts(current, NewConditions(), now)
	if _, ok := wanted["certificate-not-renewed/old.example.com"]; !ok || len(wanted) != 1 {
		t.Errorf("alerts %v", wanted)
	}
	current.Config.Spec.ACME.RenewBefore = "100d"
	if _, ok := wantedAlerts(current, NewConditions(), now)["renew-before-ignored"]; !ok {
		t.Error("a renew_before longer than the lifetime raised no alert")
	}
}

// fakeChannel keeps what it got; it fails while failing is set.
type fakeChannel struct {
	mu       sync.Mutex
	messages []notify.Message
	failing  bool
}

func (f *fakeChannel) Send(_ context.Context, message notify.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failing {
		return errors.New("the channel is down")
	}
	f.messages = append(f.messages, message)
	return nil
}

func (f *fakeChannel) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var got []string
	for _, message := range f.messages {
		got = append(got, message.Event+" "+message.Key)
	}
	return got
}

// newTestAlerts is a leader with two channels: a webhook for every alert
// and a discord for critical ones only; each fake gets what its channel
// would.
func newTestAlerts(t *testing.T) (*Alerts, *leader, *fakeChannel, *fakeChannel) {
	t.Helper()
	l := newLeader(t, 100<<30)
	var config clusterdir.Config
	var revision int64
	l.Read(func(current *state.State) { config, revision = current.Config.Spec, current.ResourcesRevision })
	config.Alerts = &clusterdir.Alerts{Channels: []clusterdir.AlertChannel{
		{Type: "webhook", URL: &clusterdir.SecretRef{Secret: "hook"}},
		{Type: "discord", Webhook: &clusterdir.SecretRef{Secret: "discord"}, MinSeverity: clusterdir.SeverityCritical},
	}}
	l.apply(state.KindApplyConfig, state.UserActor("alice"), state.ApplyConfig{
		Spec: config, ResourcesRevision: revision,
		Secrets: map[string]state.Sealed{"token": state.Sealed("t"), "hook": state.Sealed("h"), "discord": state.Sealed("d")},
	})
	webhook, discord := &fakeChannel{}, &fakeChannel{}
	open := func(sealed state.Sealed) ([]byte, error) { return sealed, nil }
	alerts := NewAlerts(l, l.propose, open, NewConditions(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	alerts.connect = func(channel clusterdir.AlertChannel, secrets notify.Secrets, _ map[string][]byte) (notify.Notifier, error) {
		if channel.Type == "webhook" {
			if url, err := secrets("hook"); err != nil || url != "h" {
				t.Errorf("the webhook's URL %q, %v", url, err)
			}
			return webhook, nil
		}
		return discord, nil
	}
	return alerts, l, webhook, discord
}

// reportPool reports node0's pool at a use.
func reportPool(l *leader, percent int) {
	l.apply(state.KindReportHost, state.HostActor(0), state.ReportHost{
		Host: 0, Version: "1.0.0",
		Pools: map[string]state.Pool{"vg-fast/thin": {Name: "vg-fast/thin", SizeBytes: 100 << 30, DataPercent: percent}},
	})
}

func TestAnAlertIsRaisedDeliveredAndResolved(t *testing.T) {
	alerts, l, webhook, discord := newTestAlerts(t)
	key := "pool-use/node0/vg-fast/thin/data"
	reportPool(l, 85)
	alerts.pass(context.Background())
	alerts.pass(context.Background()) // nothing new: nothing sent
	if got := webhook.got(); fmt.Sprint(got) != "[raised "+key+"]" || len(discord.got()) != 0 {
		t.Errorf("webhook %v, discord %v", got, discord.got())
	}
	reportPool(l, 96) // worse: raised again, now critical, so discord too
	alerts.pass(context.Background())
	if got := discord.got(); fmt.Sprint(got) != "[raised "+key+"]" || len(webhook.got()) != 2 {
		t.Errorf("webhook %v, discord %v", webhook.got(), got)
	}
	reportPool(l, 10)
	alerts.pass(context.Background())
	l.Read(func(current *state.State) {
		if len(current.Alerts) != 0 {
			t.Errorf("open alerts %v", current.Alerts)
		}
	})
	if got := webhook.got(); got[len(got)-1] != "resolved "+key {
		t.Errorf("webhook %v", got)
	}
}

func TestAChannelForCriticalAlertsGetsTheAllClearAfterADowngrade(t *testing.T) {
	alerts, l, webhook, discord := newTestAlerts(t)
	key := "pool-use/node0/vg-fast/thin/data"
	for _, percent := range []int{96, 85, 10} { // critical, warning, gone
		reportPool(l, percent)
		alerts.pass(context.Background())
	}
	if got := fmt.Sprint(discord.got()); got != "[raised "+key+" resolved "+key+"]" {
		t.Errorf("discord got %s", got)
	}
	// getting better sends nothing until the all-clear
	if got := fmt.Sprint(webhook.got()); got != "[raised "+key+" resolved "+key+"]" {
		t.Errorf("the webhook got %s", got)
	}
}

func TestAnOpenAlertStaysUntilItsConditionIsLookedAt(t *testing.T) {
	alerts, l, _, _ := newTestAlerts(t)
	l.apply(state.KindPutAlert, state.LeaderActor, state.PutAlert{
		Key: ingressNotHeld, Kind: ingressNotHeld, Severity: clusterdir.SeverityCritical, Subject: "s",
	})
	alerts.pass(context.Background()) // a new leader: the ingress loop has not looked yet
	open := func() bool {
		var ok bool
		l.Read(func(current *state.State) { _, ok = current.Alerts[ingressNotHeld] })
		return ok
	}
	if !open() {
		t.Fatal("the alert went before anyone looked")
	}
	alerts.conditions.Hold(ingressNotHeld, true) // still not held, for a moment only
	alerts.pass(context.Background())
	if !open() {
		t.Fatal("the alert went while its condition held")
	}
	alerts.conditions.Hold(ingressNotHeld, false)
	alerts.pass(context.Background())
	if open() {
		t.Error("the alert stayed after the Floating IP was held again")
	}
}

func TestTheWaitBeforeARetryStaysAnHourAfterManyFailures(t *testing.T) {
	alerts, l, webhook, _ := newTestAlerts(t)
	webhook.failing = true
	reportPool(l, 85)
	alerts.pass(context.Background())
	alerts.mu.Lock()
	alerts.outbox[0].attempts = 40
	alerts.outbox[0].next = time.Now()
	alerts.mu.Unlock()
	alerts.pass(context.Background())
	alerts.mu.Lock()
	wait := time.Until(alerts.outbox[0].next)
	alerts.mu.Unlock()
	if wait < 59*time.Minute || wait > lastRetry {
		t.Errorf("waits %v after 41 failures, want an hour", wait)
	}
}

func TestAFailedDeliveryIsTriedAgainAndShows(t *testing.T) {
	alerts, l, webhook, _ := newTestAlerts(t)
	webhook.failing = true
	reportPool(l, 85)
	alerts.pass(context.Background())
	failures := alerts.Failures()
	if len(failures) != 1 || failures[0].Attempts != 1 || failures[0].Channel != "alerts.channels[0] (webhook)" ||
		!strings.Contains(failures[0].LastError, "down") {
		t.Fatalf("failures %+v", failures)
	}
	webhook.mu.Lock()
	webhook.failing = false
	webhook.mu.Unlock()
	alerts.pass(context.Background()) // the retry is not due yet
	if len(webhook.got()) != 0 {
		t.Error("tried again before the wait was over")
	}
	alerts.mu.Lock()
	alerts.outbox[0].next = time.Now()
	alerts.mu.Unlock()
	alerts.pass(context.Background())
	if len(webhook.got()) != 1 || len(alerts.Failures()) != 0 {
		t.Errorf("after the retry: sent %v, failures %+v", webhook.got(), alerts.Failures())
	}
}

func TestANewLeaderRemindsOfEachOpenAlertOnce(t *testing.T) {
	alerts, l, webhook, _ := newTestAlerts(t)
	reportPool(l, 85)
	alerts.pass(context.Background())
	alerts.forget() // it led no more, then leads again: as a new leader would
	alerts.pass(context.Background())
	alerts.pass(context.Background())
	if got := fmt.Sprint(webhook.got()); got != "[raised pool-use/node0/vg-fast/thin/data reminder pool-use/node0/vg-fast/thin/data]" {
		t.Errorf("sent %s", got)
	}
}

func TestTheTestAlertGoesToEveryChannel(t *testing.T) {
	alerts, _, webhook, discord := newTestAlerts(t)
	discord.failing = true
	results, err := alerts.Test(context.Background())
	if err != nil || len(results) != 2 || results[0].Err != nil || results[1].Err == nil {
		t.Fatalf("results %+v, %v", results, err)
	}
	if got := webhook.got(); fmt.Sprint(got) != "[test test]" {
		t.Errorf("webhook %v", got)
	}
}
