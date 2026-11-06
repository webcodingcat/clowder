package stackspec

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/webcodingcat/clowder/internal/names"
)

// shop is the example of the README: a database and an app in front.
const shop = `
priority: high
services:
  db:
    image: postgres:17
    env: {POSTGRES_USER: shop, PORT: 5432, RATIO: 1.0, DEBUG: yes}
    secretEnv: {POSTGRES_PASSWORD: {name: db, key: password}}
    volumes: [{volume: data, path: /var/lib/postgresql/data}]
    resources: {reserve: {memory: 512M, cpus: 0.5}}
    healthcheck: {command: [pg_isready], interval: 10s}
  app:
    image: registry.example.com/shop:1.4
    dependsOn: [db]
    secrets: [db]
    http: {host: shop.example.com, port: 8080}
    ports: [{port: 9987, protocol: udp}]
`

func decodeShop(t *testing.T) Spec {
	t.Helper()
	spec, err := Decode([]byte(shop))
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

func TestTheExampleIsAStackClowderRuns(t *testing.T) {
	spec := decodeShop(t)
	if problems := Check(spec); len(problems) > 0 {
		t.Fatal(problems)
	}
	if got := spec.PublishedPorts(); !slices.Equal(got, []string{"udp/9987"}) {
		t.Errorf("ports %v", got)
	}
	if got := spec.Routes(); !slices.Equal(got, []string{"shop.example.com/"}) {
		t.Errorf("routes %v", got)
	}
	if got := spec.VolumeNames(); !slices.Equal(got, []string{"data"}) {
		t.Errorf("volumes %v", got)
	}
	if got := spec.SecretKeyRefs(); !slices.Equal(got, []names.SecretKeyRef{{Name: "db", Key: "password"}}) {
		t.Errorf("secret key refs %v", got)
	}
	if got := spec.SecretNames(); !slices.Equal(got, []string{"db"}) {
		t.Errorf("secrets %v", got)
	}
}

func TestEnvKeepsItsScalarsAsWritten(t *testing.T) {
	env := decodeShop(t).Services["db"].Env
	for variable, want := range map[string]string{"PORT": "5432", "RATIO": "1.0", "DEBUG": "yes"} {
		if env[variable] != want {
			t.Errorf("%s = %q, want %q", variable, env[variable], want)
		}
	}
}

func TestAFieldTheSchemaDoesNotHaveIsAnError(t *testing.T) {
	for _, text := range []string{
		"services: {app: {image: x, build: .}}",
		"services: {app: {image: x, network_mode: host}}",
		"services: {app: {image: x}}\nvolumes: {data: {}}",
		"services: {app: {image: x, volumes: [{volume: data, path: /data, driver: local}]}}",
	} {
		if _, err := Decode([]byte(text)); err == nil || !strings.Contains(err.Error(), "not found") {
			t.Errorf("%q: %v", text, err)
		}
	}
	if _, err := Decode(nil); err == nil {
		t.Error("an empty stack was read")
	}
}

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

func TestASecondYAMLDocumentIsAnError(t *testing.T) {
	if _, err := Decode([]byte("services: {app: {image: x}}\n---\nservices: {db: {image: y}}\n")); err == nil ||
		!strings.Contains(err.Error(), "more than one YAML document") {
		t.Errorf("error %v", err)
	}
}

func TestNormalizedSpecsAreEqualWhenTheyMeanTheSame(t *testing.T) {
	first, err := Decode([]byte(`
services:
  app:
    image: x
    command: [run, --fast]
    ports: [{port: 81}, {port: 80, published: 8080, protocol: tcp}]
    http: {host: Shop.Example.com, port: 80}
    capAdd: [NET_ADMIN, cap_chown, NET_ADMIN]
    secrets: [b, a]
    env: {}
`))
	if err != nil {
		t.Fatal(err)
	}
	second, err := Decode([]byte(`
services:
  app:
    image: x
    command: [run, --fast]
    restart: always
    restartUnhealthy: true
    ports: [{port: 80, published: 8080}, {port: 81, published: 81, protocol: tcp}]
    http: {host: shop.example.com, port: 80, path: /}
    capAdd: [CHOWN, NET_ADMIN]
    secrets: [a, b]
`))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(Normalize(first), Normalize(second)) {
		t.Errorf("\n%+v\n%+v", Normalize(first).Services["app"], Normalize(second).Services["app"])
	}
	if !reflect.DeepEqual(Normalize(Normalize(first)), Normalize(first)) {
		t.Error("a normalized spec does not normalize to itself")
	}
	normalizedHTTP := Normalize(first).Services["app"].HTTP
	normalizedHTTP.Port = 1
	if first.Services["app"].HTTP.Port != 80 {
		t.Error("the normalized spec shares its HTTP route with the one it came from")
	}
	reversed := second
	reversed.Services = map[string]Service{"app": second.Services["app"]}
	app := reversed.Services["app"]
	app.Command = []string{"--fast", "run"}
	reversed.Services["app"] = app
	if reflect.DeepEqual(Normalize(first), Normalize(reversed)) {
		t.Error("the order of a command's words is lost")
	}
	if first.Services["app"].Restart != "" {
		t.Error("Normalize changed the spec it got")
	}
}

// The state compares a stored spec with a new one by their JSON: one read
// back from a snapshot must normalize to the same JSON as before.
func TestANormalizedSpecSurvivesJSON(t *testing.T) {
	normalized := Normalize(decodeShop(t))
	encoded, err := json.Marshal(normalized)
	if err != nil {
		t.Fatal(err)
	}
	var readBack Spec
	if err := json.Unmarshal(encoded, &readBack); err != nil {
		t.Fatal(err)
	}
	again, err := json.Marshal(Normalize(readBack))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encoded, again) {
		t.Errorf("\n%s\n%s", encoded, again)
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

func TestPrivilegesAreWhatReachesBeyondTheContainer(t *testing.T) {
	service := Service{
		CapAdd: []string{"cap_chown", "NET_ADMIN", "SYS_ADMIN"}, Privileged: true,
		SecurityOpt: []string{"no-new-privileges", "seccomp=unconfined"},
	}
	want := []string{"privileged", "capAdd SYS_ADMIN", "securityOpt seccomp=unconfined"}
	if got := service.Privileges(); !slices.Equal(got, want) {
		t.Errorf("privileges %v, want %v", got, want)
	}
	if (Spec{Services: map[string]Service{"a": {}, "b": service}}).Privileged() != true {
		t.Error("a stack with a privileged service is not privileged")
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
