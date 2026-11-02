package controller

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/webcodingcat/clowder/internal/clusterspec"
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
		{80, nil, clusterspec.SeverityWarning},
		{77, open(clusterspec.SeverityWarning), clusterspec.SeverityWarning}, // hovers at the threshold: stays
		{74, open(clusterspec.SeverityWarning), ""},
		{95, open(clusterspec.SeverityWarning), clusterspec.SeverityCritical},
		{91, open(clusterspec.SeverityCritical), clusterspec.SeverityCritical},
		{89, open(clusterspec.SeverityCritical), clusterspec.SeverityWarning}, // back to warning
		{76, open(clusterspec.SeverityCritical), clusterspec.SeverityWarning},
	} {
		got, _ := leveled(test.value, test.open, poolLevels)
		if got != test.want {
			t.Errorf("%d %% with %+v open: %q, want %q", test.value, test.open, got, test.want)
		}
	}
}

// node0 is an active host with the use of its pool vg/thin and its root
// file system.
func node0(phase state.HostPhase, dataPercent, metadataPercent, rootPercent int) *state.State {
	return &state.State{Hosts: map[state.HostID]*state.Host{0: {
		ID: 0, Name: "node0", Phase: phase, RootUsedPercent: rootPercent,
		Pools: map[string]state.Pool{"vg/thin": {SizeBytes: 1, DataPercent: dataPercent, MetadataPercent: metadataPercent}},
	}}}
}

// alertKeys are the keys of the wanted alerts with their severity, sorted.
func alertKeys(current *state.State, conditions *Conditions, now time.Time) []string {
	var keys []string
	for key, alert := range wantedAlerts(current, conditions, now) {
		keys = append(keys, key+"="+alert.Severity)
	}
	slices.Sort(keys)
	return keys
}

func TestConditionsRaiseAlertsOnlyOnceTheyLasted(t *testing.T) {
	current := node0(state.Active, 96, 10, 91)
	current.Stacks = map[string]*state.Stack{
		"default/shop": {
			Namespace: "default", Name: "shop", WaitReason: "no room", WaitingSince: time.Now().Add(-6 * time.Minute),
		},
		"default/blog": {
			Namespace: "default", Name: "blog", WaitReason: "no room", WaitingSince: time.Now().Add(-time.Minute),
		},
	}
	conditions := NewConditions()
	conditions.Observe(dockerDownKey(0), true)
	conditions.Observe(ingressNotHeld, true)
	conditions.Observe(overCapacityKey(0), true)
	soon := fmt.Sprint(alertKeys(current, conditions, time.Now()))
	want := "[over-capacity/node0=warning pool-use/node0/vg/thin/data=critical root-use/node0=warning stack-waiting/default/shop=warning]"
	if soon != want {
		t.Errorf("at once %s, want %s", soon, want)
	}
	later := alertKeys(current, conditions, time.Now().Add(6*time.Minute))
	for _, key := range []string{"docker-down/node0=critical", "ingress-not-held=critical", "stack-waiting/default/blog=warning"} {
		if !slices.Contains(later, key) {
			t.Errorf("no %s after six minutes: %v", key, later)
		}
	}
}

func TestAFailingStackRaisesAnAlertWithItsErrorOnceItLasted(t *testing.T) {
	current := node0(state.Active, 0, 0, 0)
	current.Stacks = map[string]*state.Stack{"default/shop": {
		Namespace: "default", Name: "shop", Status: state.StackFailing, UpError: "pull access denied",
	}}
	conditions := NewConditions()
	conditions.Observe(stackFailingKey("default/shop"), true)
	if keys := alertKeys(current, conditions, time.Now()); len(keys) != 0 {
		t.Errorf("at once %v", keys)
	}
	alert, ok := wantedAlerts(current, conditions, time.Now().Add(stackFailingFor))["stack-failing/default/shop"]
	if !ok || alert.Severity != clusterspec.SeverityCritical || !strings.Contains(alert.Message, "pull access denied") {
		t.Errorf("once it lasted: %+v, %v", alert, ok)
	}
}

func TestAConditionRaisesItsAlertExactlyWhenItLasted(t *testing.T) {
	current := node0(state.Active, 0, 0, 0)
	conditions := NewConditions()
	conditions.Observe(dockerDownKey(0), true)
	since, _ := conditions.Since(dockerDownKey(0))
	if _, ok := wantedAlerts(current, conditions, since.Add(dockerDownFor-time.Second))["docker-down/node0"]; ok {
		t.Error("raised a second early")
	}
	if _, ok := wantedAlerts(current, conditions, since.Add(dockerDownFor))["docker-down/node0"]; !ok {
		t.Error("not raised once it lasted")
	}
}

func TestAnOpenAlertStaysWhileItsConditionHoldsOrIsNotLookedAt(t *testing.T) {
	tests := []struct {
		name string
		// look is what the loop that holds the condition saw; nil: it has
		// not looked since this agent leads
		look *bool
		want bool
	}{
		{"not looked at", nil, true},
		{"holds, not yet for long", new(true), true},
		{"ended", new(false), false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			current := node0(state.Active, 0, 0, 0)
			current.Alerts = map[string]*state.Alert{"docker-down/node0": {Severity: clusterspec.SeverityCritical}}
			conditions := NewConditions()
			if test.look != nil {
				conditions.Observe(dockerDownKey(0), *test.look)
			}
			_, ok := wantedAlerts(current, conditions, time.Now())["docker-down/node0"]
			if ok != test.want {
				t.Errorf("wanted %v, want %v", ok, test.want)
			}
		})
	}
}

func TestARemovedHostRaisesNoAlerts(t *testing.T) {
	current := node0(state.Removed, 99, 99, 99)
	conditions := NewConditions()
	conditions.Observe(dockerDownKey(0), true)
	conditions.Observe(overCapacityKey(0), true)
	if keys := alertKeys(current, conditions, time.Now().Add(time.Hour)); len(keys) != 0 {
		t.Errorf("alerts for a removed host: %v", keys)
	}
}

func TestAnAlertDoesNotChangeWithTheValueThatRaisedIt(t *testing.T) {
	// each change of an alert is a Raft write: a value that moves within
	// a level must not change it
	before := wantedAlerts(node0(state.Active, 85, 81, 91), NewConditions(), time.Now())
	after := wantedAlerts(node0(state.Active, 88, 84, 97), NewConditions(), time.Now())
	if len(before) != 3 {
		t.Fatalf("want the data, the metadata and the root file system: %v", before)
	}
	for key, alert := range before {
		if after[key] != alert {
			t.Errorf("%s changed from %+v to %+v", key, alert, after[key])
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

func TestACertificateAlertComesWhenHalfOfTheConfiguredWindowIsLeft(t *testing.T) {
	notAfter := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	current := &state.State{Certificates: map[string]*state.Certificate{
		"shop.example.com": {Domain: "shop.example.com", NotBefore: notAfter.AddDate(0, 0, -90), NotAfter: notAfter},
	}}
	current.Config.Spec.ACME.RenewBefore = "20d"
	key := "certificate-not-renewed/shop.example.com"
	halfWindow := notAfter.AddDate(0, 0, -10)
	if _, ok := wantedAlerts(current, NewConditions(), halfWindow.Add(-time.Second))[key]; ok {
		t.Error("raised before half of the window was gone")
	}
	wanted := wantedAlerts(current, NewConditions(), halfWindow)
	if _, ok := wanted[key]; !ok || len(wanted) != 1 {
		t.Errorf("alerts %v once half of the window is gone", wanted)
	}
}
