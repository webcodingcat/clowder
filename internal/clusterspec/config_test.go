package clusterspec

import (
	"errors"
	"strings"
	"testing"
)

func TestPriorityDefaultsAreCheckedAndFilledIn(t *testing.T) {
	tests := map[string]struct {
		priorities  Priorities
		wantProblem string
		wantDefault string
	}{
		"nothing":                     {Priorities{}, "", "normal"},
		"default of the defaults":     {Priorities{Default: "high"}, "", "high"},
		"misspelled default":          {Priorities{Default: "hihg"}, `"hihg" is not one of`, ""},
		"own categories, no default":  {Priorities{Categories: map[string]int{"top": 900}}, "default is required", ""},
		"own categories with default": {Priorities{Default: "top", Categories: map[string]int{"top": 900}}, "", "top"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			problems := checkPriorities(test.priorities)
			if test.wantProblem == "" {
				if len(problems) > 0 {
					t.Fatal(problems)
				}
				if got := test.priorities.withDefaults().Default; got != test.wantDefault {
					t.Errorf("default %q, want %q", got, test.wantDefault)
				}
				return
			}
			if len(problems) == 0 || !strings.Contains(problems.Error(), test.wantProblem) {
				t.Errorf("want a problem with %q, got %v", test.wantProblem, problems)
			}
		})
	}
}

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
	if config.SystemReserve != (Resources{Memory: "1G", CPUs: "0.25"}) || config.Defaults.Reservations != DefaultReservations {
		t.Errorf("system reserve %+v, reservations %+v", config.SystemReserve, config.Defaults.Reservations)
	}
}

func TestTheHostersAPIIsAFullURL(t *testing.T) {
	for api, valid := range map[string]bool{
		"":                             true,
		"https://api.hetzner.cloud/v1": true,
		"http://198.18.0.1/v1":         true,
		"api.hetzner.cloud/v1":         false,
		"ftp://api.example.com":        false,
		"https:///v1":                  false,
	} {
		hoster := Hoster{Type: "hetzner", Token: SecretRef{Secret: "token"}, API: api}
		if problems := checkHoster(hoster); (len(problems) == 0) != valid {
			t.Errorf("api %q: valid %v, problems %v", api, valid, problems)
		}
	}
}

func TestCheckReportsEveryProblem(t *testing.T) {
	config := Config{
		Seeds:   []string{"10.0.0.1:99999"},
		Hoster:  Hoster{Type: "aws", API: "api.example.com/v1"},
		Network: "both",
		Ingress: Ingress{FloatingIP: "2001:db8::1"},
		StorageClasses: map[string]StorageClass{
			"fast":  {Pool: "nopool", Reserve: "95%"},
			"any":   {Fallback: []string{"slow"}},
			"empty": {},
		},
		Replication: Replication{ResyncRate: "fast"},
		Priorities:  Priorities{Default: "medium", Categories: map[string]int{"top": 2000}},
		Stacks:      Stacks{Privileged: []string{"Monitoring"}},
		Defaults:    Defaults{Reservations: Resources{Memory: "lots", CPUs: "-1"}},
		Backup:      &Backup{Target: BackupTarget{Type: "borg"}},
		Alerts: &Alerts{
			Channels: []AlertChannel{
				{Type: "discord"},
				{Type: "pager"},
				{Type: "webhook", MinSeverity: "loud"},
				{Type: "email", SMTP: &SMTP{}, From: "clowder@example.com", To: []string{"ops@example.com"}},
			},
			Heartbeat: &Heartbeat{Interval: "often"},
		},
		ACME: ACME{Directory: "acme.example.com", Email: "ops", CA: "../ca.pem", RenewBefore: "soon"},
	}
	want := []string{
		`acme.directory "acme.example.com"`, `acme.email "ops"`, `"../ca.pem" is not a path`, `renew_before "soon"`,
		"name is required", `"10.0.0.1:99999"`, `hoster.type "aws"`, "hoster.token",
		`hoster.api "api.example.com/v1" is not a full URL`,
		`network "both"`, "not an IPv4 address", `pool "nopool"`, `reserve "95%"`,
		`falls back to "slow"`, "storage class empty", "resync_rate", "top: 2000",
		`default "medium"`, `"Monitoring" is not <namespace>/<stack>`, "reservations.memory", "reservations.cpus", `"borg"`,
		"webhook: {secret", "type is not discord", `min_severity "loud"`, "url: {secret",
		"heartbeat.url", `interval "often"`, "backup.target.url", "backup.password",
		"smtp needs host and port",
	}
	problems := Check(config)
	text := problems.Error()
	for _, fragment := range want {
		if !strings.Contains(text, fragment) {
			t.Errorf("missing %q in:\n%s", fragment, text)
		}
	}
	if len(problems) != len(want) {
		t.Errorf("want %d problems, got %d:\n%s", len(want), len(problems), text)
	}
}

func TestProblemsAreAnError(t *testing.T) {
	var err error = Problems{"a"}
	var problems Problems
	if !errors.As(err, &problems) {
		t.Error("Problems is not usable with errors.As")
	}
}
