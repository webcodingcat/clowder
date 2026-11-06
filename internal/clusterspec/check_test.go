package clusterspec

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/pki"
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

// problemsWith are the problems of Check that start with prefix.
func problemsWith(config Config, prefix string) []string {
	var found []string
	for _, problem := range Check(config) {
		if strings.HasPrefix(problem, prefix) {
			found = append(found, problem)
		}
	}
	return found
}

func TestHetznersAPIIsAFullURL(t *testing.T) {
	for api, valid := range map[string]bool{
		"":                             true,
		"https://api.hetzner.cloud/v1": true,
		"http://198.18.0.1/v1":         true,
		"api.hetzner.cloud/v1":         false,
		"ftp://api.example.com":        false,
		"https:///v1":                  false,
	} {
		problems := problemsWith(Config{Hetzner: Hetzner{API: api}}, "hetzner.api")
		if (len(problems) == 0) != valid {
			t.Errorf("api %q: valid %v, problems %v", api, valid, problems)
		}
	}
}

func TestServiceURLsAreWarnedAboutAndHTTPAboveAll(t *testing.T) {
	if warnings := ServiceURLWarnings(Config{}); len(warnings) != 0 {
		t.Errorf("the default service URLs warn: %v", warnings)
	}
	warnings := ServiceURLWarnings(Config{
		Hetzner: Hetzner{API: "http://198.18.0.1/v1"},
		ACME:    ACME{Directory: "https://ca.example.com/directory"},
	})
	if len(warnings) != 2 ||
		!strings.Contains(warnings[0], "hetzner.api is set") || !strings.Contains(warnings[0], "the Hetzner token crosses") ||
		!strings.Contains(warnings[1], "acme.directory is set") || strings.Contains(warnings[1], "unencrypted") {
		t.Errorf("warnings %q", warnings)
	}
}

func TestThePrivateNetworkIsAnIPv4NetworkOutsideClowders(t *testing.T) {
	for network, problem := range map[string]string{
		"10.0.0.0/16":  "",
		"":             "is not an IPv4 network",
		"10.0.0.1/16":  "is not an IPv4 network",
		"fd00::/8":     "is not an IPv4 network",
		"10.77.0.0/16": "overlaps clowder's 10.77.0.0/24",
	} {
		reported := problemsWith(Config{PrivateNetwork: network}, "privateNetwork")
		if problem == "" && len(reported) != 0 ||
			problem != "" && (len(reported) != 1 || !strings.Contains(reported[0], problem)) {
			t.Errorf("privateNetwork %q: %v, want %q", network, reported, problem)
		}
	}
}

// testCA is a CA certificate in PEM, as a test lab's.
func testCA(t *testing.T) string {
	t.Helper()
	ca, err := pki.NewCA("test", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	return string(ca.CertificatePEM())
}

func TestACAIsPEMWithACertificate(t *testing.T) {
	for ca, problem := range map[string]string{
		"":                               "",
		testCA(t):                        "",
		"not a certificate":              "holds no PEM certificate",
		strings.Repeat("x", MaxCASize+1): "larger than 64 KiB",
	} {
		reported := problemsWith(Config{ACME: ACME{CA: ca}}, "acme.ca")
		if problem == "" && len(reported) != 0 ||
			problem != "" && (len(reported) != 1 || !strings.Contains(reported[0], problem)) {
			t.Errorf("acme.ca of %d bytes: %v, want %q", len(ca), reported, problem)
		}
	}
}

func TestAStorageClassHasEitherAPoolOrAFallbackToOne(t *testing.T) {
	tests := map[string]struct {
		classes map[string]StorageClass
		want    string
	}{
		"pool":              {map[string]StorageClass{"fast": {Pool: "vg/thin", Reserve: "10%"}}, ""},
		"fallback to pool":  {map[string]StorageClass{"fast": {Pool: "vg/thin"}, "any": {Fallback: []string{"fast"}}}, ""},
		"no class":          {nil, "at least one class is required"},
		"pool and fallback": {map[string]StorageClass{"fast": {Pool: "vg/thin", Fallback: []string{"slow"}}}, "either pool or fallback"},
		"half a pool":       {map[string]StorageClass{"fast": {Pool: "vg/"}}, "is not volume-group/thin-pool"},
		"long volume group": {map[string]StorageClass{"fast": {Pool: strings.Repeat("v", names.MaxVolumeGroupLength+1) + "/thin"}}, "is longer than"},
		"reserve above 90%": {map[string]StorageClass{"fast": {Pool: "vg/thin", Reserve: "91%"}}, "is not a percentage"},
		"reserve no unit":   {map[string]StorageClass{"fast": {Pool: "vg/thin", Reserve: "10"}}, "is not a percentage"},
		"fallback chain": {
			map[string]StorageClass{"fast": {Pool: "vg/thin"}, "any": {Fallback: []string{"fast"}}, "last": {Fallback: []string{"any"}}},
			`falls back to "any"`,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			problems := checkStorageClasses(test.classes)
			if test.want == "" && len(problems) != 0 ||
				test.want != "" && (len(problems) != 1 || !strings.Contains(problems[0], test.want)) {
				t.Errorf("problems %q, want %q", problems, test.want)
			}
		})
	}
}

func TestAChannelNeedsTheFieldsOfItsTypeAndNoOthers(t *testing.T) {
	problems := checkChannel(0, AlertChannel{Type: "email", SMTP: &SMTP{Host: "mail.example.com", Port: 25}})
	if len(problems) != 1 || !strings.Contains(problems[0], "smtp, from and to are required") {
		t.Errorf("problems %q", problems)
	}
	problems = checkChannel(0, AlertChannel{Type: "discord", URLSecretRef: &names.SecretKeyRef{Name: "hook", Key: "url"}})
	if len(problems) != 2 || !strings.Contains(problems[1], "urlSecretRef is a webhook channel's") {
		t.Errorf("problems %q", problems)
	}
}

func TestCheckReportsEveryProblem(t *testing.T) {
	config := Config{
		Hetzner:        Hetzner{API: "api.example.com/v1"},
		PrivateNetwork: "10.77.0.0/16",
		Ingress:        Ingress{FloatingIP: "2001:db8::1"},
		StorageClasses: map[string]StorageClass{
			"fast":  {Pool: "nopool", Reserve: "95%"},
			"any":   {Fallback: []string{"slow"}},
			"empty": {},
		},
		Replication: Replication{ResyncRate: "fast"},
		Priorities:  Priorities{Default: "medium", Categories: map[string]int{"top": 2000}},
		Stacks:      Stacks{Privileged: []string{"Monitoring"}},
		Defaults:    Defaults{Reservations: Resources{Memory: "lots", CPUs: -1}},
		Alerts: &Alerts{
			Channels: []AlertChannel{
				{Type: "discord"},
				{Type: "pager"},
				{Type: "webhook", MinSeverity: "loud", WebhookSecretRef: &names.SecretKeyRef{Name: "hook", Key: "url"}},
				{
					Type: "email", SMTP: &SMTP{PasswordSecretRef: &names.SecretKeyRef{Name: "Mail", Key: "password"}, CA: "x"},
					From: "clowder@example.com", To: []string{"ops@example.com"},
				},
				{
					Type: "discord", WebhookSecretRef: &names.SecretKeyRef{Name: "hook", Key: "url"},
					SMTP: &SMTP{Host: "mail.example.com", Port: 25},
				},
			},
			Heartbeat: &Heartbeat{Interval: "often"},
		},
		ACME: ACME{Directory: "acme.example.com", Email: "ops", CA: testCA(t), RenewBefore: "soon"},
	}
	want := []string{
		`acme.directory "acme.example.com"`, `acme.email "ops"`, `renewBefore "soon"`,
		`hetzner.api "api.example.com/v1" is not a full URL`, `hetzner.tokenSecretRef: secret name ""`,
		"privateNetwork 10.77.0.0/16 overlaps clowder's 10.77.0.0/24", "not an IPv4 address", `pool "nopool"`, `reserve "95%"`,
		`falls back to "slow"`, "storage class empty", "resyncRate", "top: 2000",
		`default "medium"`, `"Monitoring" is not <namespace>/<stack>`, "reservations.memory", "reservations.cpus",
		"webhookSecretRef is required", "type is not discord", `minSeverity "loud"`, "urlSecretRef is required",
		"webhookSecretRef is a discord channel's", `interval "often"`, "smtp needs host and port",
		"smtp.ca holds no PEM certificate", `heartbeat.urlSecretRef: secret name ""`,
		`alerts.channels[3].smtp.passwordSecretRef: secret name "Mail"`,
		"alerts.channels[4] (discord): smtp, from and to are an email channel's",
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
