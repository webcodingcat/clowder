package stackspec

import (
	"bytes"
	"encoding/json"
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
