package compose

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loadText loads a stack from Compose text written to a temporary file.
func loadText(t *testing.T, text string) (*Stack, error) {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "compose.yaml")
	if err := os.WriteFile(file, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(context.Background(), LoadOptions{Name: "test", WorkingDir: dir, Files: []string{file}})
}

func TestTheSourceReadsBackWithoutTheUsersFiles(t *testing.T) {
	dir := t.TempDir()
	write := func(name, text string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("app.env", "TOKEN=from-the-env-file\n")
	write("compose.yaml", "services:\n  app:\n    image: example/app:${TAG}\n    env_file: [app.env]\n"+
		"    environment: {PRICE: \"$$5\"}\n    x-clowder: {priority: 300}\n"+
		"volumes:\n  data:\n    x-clowder: {class: fast, size: 1G, replicas: 1}\n")
	stack, err := Load(context.Background(), LoadOptions{
		Name: "shop", WorkingDir: dir, Files: []string{filepath.Join(dir, "compose.yaml")},
		Environment: map[string]string{"TAG": "1.2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	source, err := stack.Source()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(source), dir) {
		t.Errorf("the source points into the user's directory:\n%s", source)
	}
	if err := os.RemoveAll(dir); err != nil { // the cluster has none of the user's files
		t.Fatal(err)
	}
	parsed, err := Parse(context.Background(), "shop", source)
	if err != nil {
		t.Fatalf("%v\n%s", err, source)
	}
	app := parsed.Project.Services["app"]
	environment := app.Environment
	if app.Image != "example/app:1.2" || *environment["TOKEN"] != "from-the-env-file" || *environment["PRICE"] != "$5" {
		t.Errorf("image %s, environment %v", app.Image, environment)
	}
	if parsed.Services["app"].Priority.Value == nil || *parsed.Services["app"].Priority.Value != 300 ||
		parsed.Volumes["data"].Replicas != 1 {
		t.Errorf("the x-clowder fields did not survive: %+v %+v", parsed.Services["app"], parsed.Volumes["data"])
	}
}

func TestTheShopExampleLoads(t *testing.T) {
	stack, err := Load(context.Background(), LoadOptions{
		Name:       "shop",
		WorkingDir: "testdata/shop",
		Files:      []string{"testdata/shop/compose.yaml"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if stack.Priority.Category != "high" || len(stack.Exports) != 1 || stack.Exports[0] != "db" {
		t.Errorf("stack fields: %+v", stack)
	}
	app := stack.Services["app"]
	if app.HTTP == nil || app.HTTP.Host != "shop.example.com" || app.HTTP.Port != 8080 {
		t.Errorf("app route: %+v", app.HTTP)
	}
	// cpus "0.5" stays 0.5 although compose-go calls the field NanoCPUs
	if app.MemoryReservation != 256<<20 || app.CPUReservation != 0.5 {
		t.Errorf("app reservation: %d bytes, %v CPUs", app.MemoryReservation, app.CPUReservation)
	}
	if len(app.DependsOn) != 1 || app.DependsOn[0] != "db" || !app.RestartUnhealthy {
		t.Errorf("app: %+v", app)
	}
	cache := stack.Services["cache"]
	if cache.RestartUnhealthy || cache.Restart != "on-failure:3" || len(cache.Ports) != 2 {
		t.Errorf("cache: %+v", cache)
	}
	if cache.Ports[1] != (PublishedPort{Protocol: "udp", Published: 9987, Target: 9987}) {
		t.Errorf("cache udp port: %+v", cache.Ports[1])
	}
	pgdata := stack.Volumes["pgdata"]
	if pgdata.Class != "fast" || pgdata.Size != 20<<30 || pgdata.Replicas != 2 {
		t.Errorf("pgdata: %+v", pgdata)
	}
	if pgdata.Backup == nil || pgdata.Backup.Keep != 14 || pgdata.Backup.Pre.Service != "db" {
		t.Errorf("pgdata backup: %+v", pgdata.Backup)
	}
	if _, ok := stack.Project.Services["db"].Extensions[extensionKey]; ok {
		t.Error("the project still carries x-clowder")
	}
}

func TestPrioritiesAreNumbersOrCategories(t *testing.T) {
	cases := []struct {
		raw      any
		value    int
		category string
		fails    bool
	}{
		{raw: 700.0, value: 700},
		{raw: "250", value: 250},
		{raw: "critical", category: "critical"},
		{raw: 1001.0, fails: true},
		{raw: -1.0, fails: true},
		{raw: 1.5, fails: true},
		{raw: "", fails: true},
		{raw: true, fails: true},
	}
	for _, c := range cases {
		priority, err := parsePriority(c.raw)
		switch {
		case c.fails && err == nil:
			t.Errorf("%v: no error", c.raw)
		case !c.fails && err != nil:
			t.Errorf("%v: %v", c.raw, err)
		case c.category != "" && priority.Category != c.category:
			t.Errorf("%v: category %q", c.raw, priority.Category)
		case c.category == "" && !c.fails && (priority.Value == nil || *priority.Value != c.value):
			t.Errorf("%v: value %v, want %d", c.raw, priority.Value, c.value)
		}
	}
}

func TestWhatClowderCannotRunIsReported(t *testing.T) {
	cases := map[string]struct{ compose, problem string }{
		"build": {
			"services: {web: {build: .}}", "build is not supported",
		},
		"host network": {
			"services: {web: {image: x, network_mode: host}}", `network_mode "host"`,
		},
		"pid host": {
			"services: {web: {image: x, pid: host}}", "pid: host",
		},
		"container name": {
			"services: {web: {image: x, container_name: web}}", "container_name",
		},
		"devices": {
			"services: {web: {image: x, devices: [/dev/sda]}}", "devices",
		},
		"replicas": {
			"services: {web: {image: x, deploy: {replicas: 2}}}", "more than one replica",
		},
		"bind mount": {
			"services: {web: {image: x, volumes: [/srv:/srv]}}", "bind mount /srv",
		},
		"anonymous volume": {
			"services: {web: {image: x, volumes: [/data]}}", "anonymous volume",
		},
		"port range": {
			`services: {web: {image: x, ports: ["8000-8001:80"]}}`, "exactly one published port",
		},
		"random port": {
			`services: {web: {image: x, ports: ["80"]}}`, "exactly one published port",
		},
		"host address": {
			`services: {web: {image: x, ports: ["127.0.0.1:8080:80"]}}`, "host address",
		},
		"reserved port": {
			`services: {web: {image: x, ports: ["443:443"]}}`, "reserved",
		},
		"bad restart": {
			"services: {web: {image: x, restart: sometimes}}", `restart "sometimes"`,
		},
		"unknown x-clowder key": {
			"services: {web: {image: x, x-clowder: {priorty: high}}}", `unknown field "priorty"`,
		},
		"volume without class": {
			"services: {web: {image: x}}\nvolumes: {data: {x-clowder: {size: 1G, replicas: 1}}}", "class is required",
		},
		"volume without size": {
			"services: {web: {image: x}}\nvolumes: {data: {x-clowder: {class: fast, replicas: 1}}}", "size is required",
		},
		"volume without replicas": {
			"services: {web: {image: x}}\nvolumes: {data: {x-clowder: {class: fast, size: 1G}}}", "replicas is required",
		},
		"too many replicas": {
			"services: {web: {image: x}}\nvolumes: {data: {x-clowder: {class: fast, size: 1G, replicas: 4}}}", "replicas 4",
		},
		"negative keep": {
			"services: {web: {image: x}}\nvolumes: {data: {x-clowder: {class: fast, size: 1G, replicas: 1, backup: {schedule: daily, keep: -1}}}}",
			"keep -1",
		},
		"backup hook without command": {
			"services: {web: {image: x}}\nvolumes: {data: {x-clowder: {class: fast, size: 1G, replicas: 1, backup: {schedule: daily, pre: {service: web}}}}}",
			"pre needs service and command",
		},
		"overlay network": {
			"services: {web: {image: x}}\nnetworks: {default: {driver: overlay}}", `driver "overlay"`,
		},
		"own subnet": {
			"services: {web: {image: x}}\nnetworks: {default: {ipam: {config: [{subnet: 10.77.0.0/24}]}}}", "ipam",
		},
		"secret from a file": {
			"services: {web: {image: x}}\nsecrets: {token: {file: ./token.txt}}", "secret token: file",
		},
		"host uts": {
			"services: {web: {image: x, uts: host}}", "uts: host",
		},
		"host user namespace": {
			"services: {web: {image: x, userns_mode: host}}", "userns_mode: host",
		},
		"one port twice": {
			"services: {web: {image: x, ports: [\"8080:80\"]}, api: {image: x, ports: [\"8080:81\"]}}",
			"port tcp/8080 is published by api already",
		},
		"x-clowder on a network": {
			"services: {web: {image: x}}\nnetworks: {default: {x-clowder: {class: fast}}}", "not used on networks",
		},
		"config from a file": {
			"services: {web: {image: x}}\nconfigs: {app: {file: ./app.conf}}", "config app: file",
		},
		"external volume": {
			"services: {web: {image: x}}\nvolumes: {data: {external: true}}", "external volumes",
		},
		"export of a missing service": {
			"x-clowder: {exports: [db]}\nservices: {web: {image: x}}", `exports "db"`,
		},
		"malformed import": {
			"x-clowder: {imports: [db]}\nservices: {web: {image: x}}", "stack/service",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := loadText(t, c.compose)
			var problems Problems
			if !errors.As(err, &problems) {
				t.Fatalf("want Problems, got %v", err)
			}
			if !strings.Contains(problems.Error(), c.problem) {
				t.Errorf("want %q in:\n%v", c.problem, problems)
			}
		})
	}
}

func TestAllProblemsAreReportedAtOnce(t *testing.T) {
	_, err := loadText(t, "services: {web: {build: ., container_name: web, pid: host}}")
	var problems Problems
	if !errors.As(err, &problems) || len(problems) != 3 {
		t.Errorf("want 3 problems, got %v", err)
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
