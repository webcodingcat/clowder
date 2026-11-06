package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/webcodingcat/clowder/internal/clusterspec"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/notify"
	"github.com/webcodingcat/clowder/internal/resource"
	"github.com/webcodingcat/clowder/internal/state"
)

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
	var config clusterspec.Config
	l.Read(func(current *state.State) { config = current.Config.Spec })
	config.Alerts = &clusterspec.Alerts{Channels: []clusterspec.AlertChannel{
		{Type: "webhook", URLSecretRef: &names.SecretKeyRef{Name: "alerts", Key: "webhook"}},
		{
			Type: "discord", WebhookSecretRef: &names.SecretKeyRef{Name: "alerts", Key: "discord"},
			MinSeverity: clusterspec.SeverityCritical,
		},
	}}
	putSystemSecret(l, "alerts", map[string]string{"webhook": "h", "discord": "d"})
	putConfig(l, config)
	webhook, discord := &fakeChannel{}, &fakeChannel{}
	alerts := NewAlerts(l, l.propose, openPadded, NewConditions(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	alerts.connect = func(channel clusterspec.AlertChannel, secrets notify.Secrets) (notify.Notifier, error) {
		if channel.Type == "webhook" {
			// the URL comes from the Secret of clowder-system
			if url, err := secrets(*channel.URLSecretRef); err != nil || url != "h" {
				t.Errorf("the webhook's URL %q, %v", url, err)
			}
			if _, err := secrets(names.SecretKeyRef{Name: "alerts", Key: "missing"}); err == nil {
				t.Error("a key the Secret lacks opened")
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
		Key: ingressNotHeld, Kind: ingressNotHeld, Severity: clusterspec.SeverityCritical, Subject: "s",
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
	alerts.conditions.Observe(ingressNotHeld, true) // still not held, for a moment only
	alerts.pass(context.Background())
	if !open() {
		t.Fatal("the alert went while its condition held")
	}
	alerts.conditions.Observe(ingressNotHeld, false)
	alerts.pass(context.Background())
	if open() {
		t.Error("the alert stayed after the Floating IP was held again")
	}
}

func TestTheConditionsOfGoneStacksAndHostsAreForgotten(t *testing.T) {
	alerts, l, _, _ := newTestAlerts(t)
	shop, node0 := stackFailingKey("default/shop"), dockerDownKey(0)
	alerts.conditions.Observe(shop, true)
	alerts.conditions.Observe(node0, true)
	alerts.pass(context.Background())
	if !alerts.conditions.Seen(shop) || !alerts.conditions.Seen(node0) {
		t.Fatal("a condition was forgotten while its stack and host are there")
	}
	deleteResource(l, resource.KindStack, "shop")
	withServers(l) // the last host that runs stacks cannot go
	l.apply(state.KindSetHostPhase, state.CLIActor("admin"), state.SetHostPhase{Host: 0, Phase: state.Removed})
	alerts.pass(context.Background())
	for _, key := range []string{shop, node0} {
		if _, held := alerts.conditions.Since(key); held || alerts.conditions.Seen(key) {
			t.Errorf("%s is still known after its subject went", key)
		}
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
