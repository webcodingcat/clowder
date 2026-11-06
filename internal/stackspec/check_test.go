package stackspec

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestEveryProblemIsReportedAtOnce(t *testing.T) {
	spec, err := Decode([]byte(`
priority: "2000"
services:
  web_1:
    ports: [{port: 22}, {port: 8080, protocol: sctp}]
    http: {host: localhost, port: 0, path: "a b"}
    volumes: [{volume: Data, path: data}, {volume: logs, path: /data/../logs}]
    tmpfs: [{path: /tmp, size: lots}]
    dependsOn: [web_1, nobody]
    healthcheck: {command: [], interval: soon}
    restart: unless-stopped
    resources: {limit: {memory: lots, cpus: -1}}
    stopGracePeriod: forever
    stopSignal: TERM
    shmSize: big
    env: {1A: x}
    secretEnv: {TOKEN: {name: Bad, key: k}, KEY: {name: ok, key: ..}, 1B: {name: ok, key: k}}
    secrets: [a, a, B]
    capAdd: [""]
`))
	if err != nil {
		t.Fatal(err)
	}
	problems := Check(spec)
	for _, want := range []string{
		"priority: 2000 is not from 0 to 1000",
		"service web_1: the name is not a service name",
		"service web_1: image is required",
		"ports: 22 is reserved",
		`ports: protocol "sctp" is not tcp or udp`,
		`http: host "localhost" is not a domain name`,
		"http: a port is required",
		`http: path "a b" must start with /`,
		`volumes: "Data" is not a volume name`,
		`volumes: "data" is not a clean absolute path`,
		`volumes: "/data/../logs" is not a clean absolute path`,
		`tmpfs /tmp: size "lots"`,
		`dependsOn: "web_1" is no other service`,
		`dependsOn: "nobody" is no other service`,
		"healthcheck: a command is required",
		`healthcheck: interval "soon"`,
		`restart "unless-stopped"`,
		`resources.limit.memory: size "lots"`,
		"resources.limit.cpus is negative",
		`stopGracePeriod "forever"`,
		`stopSignal "TERM"`,
		`shmSize: size "big"`,
		`env: "1A" is not a variable name`,
		`secretEnv.TOKEN: secret name "Bad"`,
		`secretEnv.KEY: key ".." of secret ok`,
		`secretEnv: "1B" is not a variable name`,
		"secrets: a secret is named twice",
		`secrets: "B" is not a secret name`,
		`capability "" is not a capability's name`,
	} {
		if !slices.ContainsFunc(problems, func(problem string) bool { return strings.Contains(problem, want) }) {
			t.Errorf("no problem with %q in:\n%v", want, problems)
		}
	}
	var asError Problems
	if !errors.As(error(problems), &asError) || !strings.Contains(problems.Error(), "cannot run on clowder") {
		t.Errorf("problems as an error: %v", problems)
	}
}

func TestTwoServicesCannotTakeTheSamePortOrRoute(t *testing.T) {
	spec, err := Decode([]byte(`
services:
  a: {image: x, ports: [{port: 9987, protocol: udp}], http: {host: Shop.example.com, port: 80}}
  b: {image: x, ports: [{port: 1, published: 9987, protocol: udp}], http: {host: shop.example.com, port: 81, path: /}}
`))
	if err != nil {
		t.Fatal(err)
	}
	problems := Check(spec)
	if len(problems) != 2 || !strings.Contains(problems[0], "port udp/9987 is a's already") ||
		!strings.Contains(problems[1], "route shop.example.com/ is a's already") {
		t.Errorf("problems %v", problems)
	}
}

func TestServicesThatDependOnEachOtherInACircleAreRefused(t *testing.T) {
	spec, err := Decode([]byte(`
services:
  a: {image: x, dependsOn: [b]}
  b: {image: x, dependsOn: [c]}
  c: {image: x, dependsOn: [a]}
  d: {image: x, dependsOn: [a]}
`))
	if err != nil {
		t.Fatal(err)
	}
	if problems := Check(spec); len(problems) != 1 || !strings.Contains(problems[0], "dependsOn goes in a circle") {
		t.Errorf("problems %v", problems)
	}
}

func TestACircleIsBlamedOnAServiceInIt(t *testing.T) {
	spec, err := Decode([]byte(`
services:
  a: {image: x, dependsOn: [b]}
  b: {image: x, dependsOn: [c]}
  c: {image: x, dependsOn: [b]}
`))
	if err != nil {
		t.Fatal(err)
	}
	if problems := Check(spec); len(problems) != 1 || !strings.HasPrefix(problems[0], "service b: dependsOn goes in a circle") {
		t.Errorf("problems %v", problems)
	}
}

func TestPrioritiesAreNumbersOrCategories(t *testing.T) {
	if priority, err := ParsePriority("700"); err != nil || *priority.Value != 700 {
		t.Errorf("700: %+v, %v", priority, err)
	}
	if priority, err := ParsePriority("high"); err != nil || priority.Category != "high" {
		t.Errorf("high: %+v, %v", priority, err)
	}
	// none written is the cluster's default, resolved when it is placed
	if priority, err := ParsePriority(""); err != nil || priority.Value != nil || priority.Category != "" {
		t.Errorf(`"": %+v, %v`, priority, err)
	}
	for _, text := range []string{"-1", "1001", "High"} {
		if _, err := ParsePriority(text); err == nil {
			t.Errorf("%q was taken", text)
		}
	}
}

func TestRestartIsARuleTheSupervisorApplies(t *testing.T) {
	for restart, valid := range map[string]bool{
		"always":         true,
		"no":             true,
		"on-failure":     true,
		"on-failure:3":   true,
		"on-failure:0":   false,
		"on-failure:-1":  false,
		"on-failure:x":   false,
		"unless-stopped": false,
		"":               false,
	} {
		if got := ValidRestart(restart); got != valid {
			t.Errorf("%q: valid %v, want %v", restart, got, valid)
		}
	}
}

func TestReservedPortsCannotBePublished(t *testing.T) {
	for _, port := range []uint16{22, 53, 80, 443, 7400, 7499, 10000, 10999, 20000, 29999, 51800, 51831} {
		if !ReservedPort(port) {
			t.Errorf("%d should be reserved", port)
		}
	}
	for _, port := range []uint16{8080, 7399, 7500, 9987, 30000, 51832} {
		if ReservedPort(port) {
			t.Errorf("%d should be free", port)
		}
	}
}
