package clusterspec

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/webcodingcat/clowder/internal/names"
)

func TestAPriorityIsANumberACategoryOrTheDefault(t *testing.T) {
	priorities := DefaultPriorities()
	number := 42
	for _, test := range []struct {
		priority Priority
		want     int
	}{
		{Priority{Value: &number}, 42}, {Priority{Category: "high"}, 700}, {Priority{}, 500},
	} {
		if got, err := priorities.Resolve(test.priority); err != nil || got != test.want {
			t.Errorf("%+v: %d, %v", test.priority, got, err)
		}
	}
	if _, err := priorities.Resolve(Priority{Category: "urgent"}); err == nil {
		t.Error("an unknown category resolved")
	}
}

func TestSeveritiesAreOrdered(t *testing.T) {
	if !AtLeast(SeverityCritical, SeverityWarning) || AtLeast(SeverityInfo, SeverityWarning) ||
		!AtLeast(SeverityInfo, "") || !AtLeast(SeverityWarning, SeverityWarning) {
		t.Error("severities out of order")
	}
}

func TestTheReservationsAndTheSystemReserveHaveDefaults(t *testing.T) {
	config := Config{SystemReserve: Resources{Memory: "1G"}}.WithDefaults()
	if config.SystemReserve != (Resources{Memory: "1G", CPUs: 0.25}) || config.Defaults.Reservations != DefaultReservations {
		t.Errorf("system reserve %+v, reservations %+v", config.SystemReserve, config.Defaults.Reservations)
	}
}

func TestCPUsAreANumberAsInAStack(t *testing.T) {
	var config Config
	if err := json.Unmarshal([]byte(`{"systemReserve": {"cpus": 0.5}}`), &config); err != nil {
		t.Fatal(err)
	}
	if config.SystemReserve.CPUs != 0.5 {
		t.Errorf("cpus %v, want 0.5", config.SystemReserve.CPUs)
	}
}

func TestTheSecretPlacesAreEveryFieldThatNamesASecret(t *testing.T) {
	ref := func(name string) *names.SecretKeyRef { return &names.SecretKeyRef{Name: name, Key: "value"} }
	config := Config{
		Hetzner: Hetzner{TokenSecretRef: names.SecretKeyRef{Name: "hetzner", Key: "token"}},
		Alerts: &Alerts{
			Channels: []AlertChannel{
				{Type: "discord", WebhookSecretRef: ref("discord")},
				{Type: "webhook", URLSecretRef: ref("hook")},
				{Type: "email", SMTP: &SMTP{PasswordSecretRef: ref("smtp")}},
				{Type: "email", SMTP: &SMTP{}},
			},
			Heartbeat: &Heartbeat{URLSecretRef: *ref("beat")},
		},
	}
	var got []string
	for _, place := range SecretPlaces(config) {
		got = append(got, place.Path+"="+place.Ref.String())
	}
	want := []string{
		"hetzner.tokenSecretRef=hetzner[token]",
		"alerts.channels[0].webhookSecretRef=discord[value]",
		"alerts.channels[1].urlSecretRef=hook[value]",
		"alerts.channels[2].smtp.passwordSecretRef=smtp[value]",
		"alerts.heartbeat.urlSecretRef=beat[value]",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("places:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
