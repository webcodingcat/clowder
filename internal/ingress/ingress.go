// Package ingress renders the files of clowder's Traefik: its Compose file,
// its static configuration and the routes of the file provider. The agent
// writes them and runs Traefik; this package only renders, so its output is
// tested against golden files.
//
// Traefik runs in the host network. Every host has the Floating IP on its
// interface, held or not, so Traefik binds it on every host and at boot;
// the hoster routes the address to one host only.
package ingress

import (
	"bytes"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Project is the Compose project of clowder's Traefik. names refuses stack
// names that start with "clowder-", so no stack can take it.
const Project = "clowder-traefik"

// Image is Traefik's image, pinned per clowder release: a new pin recreates
// the container at the agent's first pass.
const Image = "traefik:v3.7.16"

// The ports Traefik and the agent use besides 80 and 443; all lie in the
// range compose.ReservedPort keeps for clowder.
const (
	// InternalPort is the entrypoint on the node address that other hosts
	// send requests to (phase 2).
	InternalPort = 7480
	// PingPort is Traefik's health check, on 127.0.0.1.
	PingPort = 7446
)

// loopback is where only the host itself reaches: Traefik's health check.
var loopback = netip.AddrFrom4([4]byte{127, 0, 0, 1})

// Where Traefik finds its files inside the container.
const (
	staticDir  = "/etc/traefik"
	dynamicDir = "/etc/traefik-dynamic"
)

// Static is what the static configuration depends on; a change of it
// recreates the container.
type Static struct {
	FloatingIP  netip.Addr
	NodeAddress netip.Addr
	// Overlay is the node network: only its hosts may set X-Forwarded-For
	// on the internal entrypoint.
	Overlay netip.Prefix
}

// staticConfig is Traefik's static configuration (traefik.yml).
type staticConfig struct {
	Global      globalConfig          `yaml:"global"`
	Log         logConfig             `yaml:"log"`
	EntryPoints map[string]entryPoint `yaml:"entryPoints"`
	Ping        pingConfig            `yaml:"ping"`
	Providers   providersConfig       `yaml:"providers"`
}

type globalConfig struct {
	CheckNewVersion    bool `yaml:"checkNewVersion"`
	SendAnonymousUsage bool `yaml:"sendAnonymousUsage"`
}

type logConfig struct {
	Level string `yaml:"level"`
}

type entryPoint struct {
	Address          string            `yaml:"address"`
	ForwardedHeaders *forwardedHeaders `yaml:"forwardedHeaders,omitempty"`
}

type forwardedHeaders struct {
	TrustedIPs []string `yaml:"trustedIPs"`
}

type pingConfig struct {
	EntryPoint string `yaml:"entryPoint"`
}

type providersConfig struct {
	File fileProvider `yaml:"file"`
}

type fileProvider struct {
	Directory string `yaml:"directory"`
	Watch     bool   `yaml:"watch"`
}

// RenderStatic writes traefik.yml. The public entrypoints trust no
// X-Forwarded-For (Traefik then sets the client's address itself); the
// internal one trusts the node network, whose hosts pass the client's
// address on.
func RenderStatic(static Static) ([]byte, error) {
	if !static.FloatingIP.IsValid() || !static.NodeAddress.IsValid() || !static.Overlay.IsValid() {
		return nil, fmt.Errorf("render Traefik's configuration: the Floating IP, the node address and the node network are needed")
	}
	config := staticConfig{
		Log: logConfig{Level: "WARN"},
		EntryPoints: map[string]entryPoint{
			"web":       {Address: Target(static.FloatingIP, 80)},
			"websecure": {Address: Target(static.FloatingIP, 443)},
			"internal": {
				Address:          Target(static.NodeAddress, InternalPort),
				ForwardedHeaders: &forwardedHeaders{TrustedIPs: []string{static.Overlay.String()}},
			},
			"ping": {Address: Target(loopback, PingPort)},
		},
		Ping:      pingConfig{EntryPoint: "ping"},
		Providers: providersConfig{File: fileProvider{Directory: dynamicDir, Watch: true}},
	}
	return marshal("Traefik's configuration", config)
}

// Mounts are the host's directories Traefik reads.
type Mounts struct {
	// Static holds traefik.yml.
	Static string
	// Dynamic holds the routes; Traefik watches it.
	Dynamic string
}

// RenderCompose writes Traefik's Compose file. Like every container of
// clowder's, it has restart "no": the agent's supervisor restarts it, and
// after a boot nothing starts before the agent.
func RenderCompose(mounts Mounts) ([]byte, error) {
	healthcheck := []string{"CMD", "traefik", "healthcheck", "--ping", "--configFile=" + staticDir + "/traefik.yml"}
	file := map[string]any{
		"name": Project,
		"services": map[string]any{
			"traefik": map[string]any{
				"image":        Image,
				"network_mode": "host",
				"restart":      "no",
				"command":      []string{"--configFile=" + staticDir + "/traefik.yml"},
				// the label the supervisor follows
				"labels":    map[string]string{"clowder.stack": Project},
				"read_only": true,
				"volumes": []string{
					mounts.Static + ":" + staticDir + ":ro",
					mounts.Dynamic + ":" + dynamicDir + ":ro",
				},
				"healthcheck": map[string]any{
					"test": healthcheck, "interval": "10s", "timeout": "5s", "retries": 3, "start_period": "10s",
				},
			},
		},
	}
	return marshal("Traefik's Compose file", file)
}

// Route is one HTTP route of a stack and the containers that take its
// requests.
type Route struct {
	Stack, Service string
	Host, Path     string
	// Targets are "address:port" of the containers that may take requests:
	// running, and healthy if they have a health check.
	Targets []string
}

// dynamicConfig is the file provider's configuration.
type dynamicConfig struct {
	HTTP httpConfig `yaml:"http"`
}

type httpConfig struct {
	Routers  map[string]router  `yaml:"routers"`
	Services map[string]service `yaml:"services"`
}

type router struct {
	EntryPoints []string `yaml:"entryPoints"`
	Rule        string   `yaml:"rule"`
	Service     string   `yaml:"service"`
}

type service struct {
	LoadBalancer loadBalancer `yaml:"loadBalancer"`
}

type loadBalancer struct {
	// Servers is empty while no container may take requests: Traefik then
	// answers 503, not 404, so the route does not look missing.
	Servers []server `yaml:"servers"`
}

type server struct {
	URL string `yaml:"url"`
}

// RenderDynamic writes the routes for the file provider: per route a
// router on the web entrypoint and a service with its targets. A router and
// its service are named "<stack>.<service>": stack names have no dot, so
// names never clash.
func RenderDynamic(routes []Route) ([]byte, error) {
	config := dynamicConfig{HTTP: httpConfig{Routers: map[string]router{}, Services: map[string]service{}}}
	for _, route := range routes {
		name := route.Stack + "." + route.Service
		config.HTTP.Routers[name] = router{EntryPoints: []string{"web"}, Rule: rule(route), Service: name}
		servers := []server{}
		for _, target := range slices.Sorted(slices.Values(route.Targets)) {
			servers = append(servers, server{URL: "http://" + target})
		}
		config.HTTP.Services[name] = service{LoadBalancer: loadBalancer{Servers: servers}}
	}
	return marshal("Traefik's routes", config)
}

// rule matches the route's host and, unless it is "/" or empty, its path
// and below. compose.Load made sure neither holds a backtick.
func rule(route Route) string {
	matcher := "Host(`" + strings.ToLower(route.Host) + "`)"
	if route.Path != "" && route.Path != "/" {
		matcher += " && PathPrefix(`" + route.Path + "`)"
	}
	return matcher
}

// marshal writes YAML with an indent of two, as the other files clowder
// renders.
func marshal(what string, value any) ([]byte, error) {
	var out bytes.Buffer
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	if err := encoder.Encode(value); err != nil {
		return nil, fmt.Errorf("render %s: %w", what, err)
	}
	if err := encoder.Close(); err != nil {
		return nil, fmt.Errorf("render %s: %w", what, err)
	}
	return out.Bytes(), nil
}

// Target is an address and port as Traefik takes them: a route's target,
// an entrypoint's address.
func Target(address netip.Addr, port uint16) string {
	return netip.AddrPortFrom(address, port).String()
}
