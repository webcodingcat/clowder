// Package ingress renders the files of clowder's Traefik: its Compose file,
// its static configuration and the routes of the file provider. The agent
// writes them and runs Traefik; this package only renders, so its output is
// tested against golden files.
//
// Traefik runs in the host network. Every host has the Floating IP on its
// interface, whether it holds the address or not, so Traefik can bind it on
// every host and at boot. Hetzner routes the address to one host only.
package ingress

import (
	"bytes"
	"fmt"
	"net/netip"
	"path"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Project is the Compose project of clowder's Traefik. names refuses stack
// names that start with "clowder-", so no stack can use this name.
const Project = "clowder-traefik"

// Image is Traefik's image, pinned per clowder release: a new pin recreates
// the container at the agent's first pass.
const Image = "traefik:v3.7.16"

// The ports that Traefik and the agent use besides 80 and 443. All of them
// lie in the range that stackspec.ReservedPort keeps for clowder.
const (
	// InternalPort is the entrypoint on the node address that other hosts
	// send requests to (phase 2).
	InternalPort = 7480
	// PingPort is Traefik's health check, on 127.0.0.1.
	PingPort = 7446
	// ChallengePort is where the agent serves the answers to ACME's
	// HTTP-01 challenges, on 127.0.0.1.
	ChallengePort = 7445
)

// ChallengePath is the path at which ACME asks for its HTTP-01 challenges,
// followed by the token.
const ChallengePath = "/.well-known/acme-challenge/"

// loopback is the address that only the host itself can reach. Traefik's
// health check and the answers to ACME's challenges listen there.
var loopback = netip.AddrFrom4([4]byte{127, 0, 0, 1})

// ChallengeAddress is where the agent serves the answers to ACME's
// challenges and where Traefik sends the challenges.
var ChallengeAddress = Target(loopback, ChallengePort)

// Where Traefik finds its files inside the container.
const (
	staticDir       = "/etc/traefik"
	dynamicDir      = "/etc/traefik-dynamic"
	certificatesDir = "/etc/traefik-certificates"
)

// Static is what the static configuration depends on; a change to it
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
// X-Forwarded-For header, so Traefik sets the client's address itself. The
// internal entrypoint trusts the node network, whose hosts pass on the
// client's address.
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

// Mounts are the host's directories that Traefik reads.
type Mounts struct {
	// Static holds traefik.yml.
	Static string
	// Dynamic holds the routes; Traefik watches it.
	Dynamic string
	// Certificates holds the certificates and their keys, on a tmpfs.
	Certificates string
}

// CertificateFiles are the files of a domain's certificate in the
// certificates' directory. Traefik watches only the routes, so a renewed
// certificate must get new file names. Then the routes change too, and
// Traefik reads the new certificate.
type CertificateFiles struct {
	Domain string
	// Chain and Key are the files' names in the directory.
	Chain, Key string
}

// RenderCompose writes Traefik's Compose file. Like every clowder
// container, Traefik has restart "no": the agent's supervisor restarts it,
// and after a boot nothing starts before the agent.
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
					mounts.Certificates + ":" + certificatesDir + ":ro",
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
// requests. Project is the stack's Compose project (names.Project).
type Route struct {
	Project, Service string
	Host, Path       string
	// Targets are the "address:port" of the containers that may take
	// requests: those that run and, if they have a health check, are
	// healthy. For a route of a stack on another host, the target is that
	// host's internal entrypoint.
	Targets []string
	// Forwarded says that the stack runs on another host. The route sends
	// its requests to that host's Traefik, and only from the public
	// entrypoints, so a request is forwarded at most once.
	Forwarded bool
}

// dynamicConfig is the file provider's configuration.
type dynamicConfig struct {
	HTTP httpConfig `yaml:"http"`
	TLS  *tlsConfig `yaml:"tls,omitempty"`
}

type httpConfig struct {
	Routers     map[string]router     `yaml:"routers"`
	Middlewares map[string]middleware `yaml:"middlewares"`
	Services    map[string]service    `yaml:"services"`
}

type router struct {
	EntryPoints []string   `yaml:"entryPoints"`
	Rule        string     `yaml:"rule"`
	Priority    int        `yaml:"priority,omitempty"`
	Middlewares []string   `yaml:"middlewares,omitempty"`
	Service     string     `yaml:"service"`
	TLS         *routerTLS `yaml:"tls,omitempty"`
}

// routerTLS is empty: Traefik picks the certificate from those in tlsConfig
// by the client's SNI.
type routerTLS struct{}

type middleware struct {
	RedirectScheme redirectScheme `yaml:"redirectScheme"`
}

type redirectScheme struct {
	Scheme    string `yaml:"scheme"`
	Permanent bool   `yaml:"permanent"`
}

type tlsConfig struct {
	Certificates []tlsCertificate `yaml:"certificates"`
}

type tlsCertificate struct {
	CertFile string `yaml:"certFile"`
	KeyFile  string `yaml:"keyFile"`
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

// The names of the router, middleware and service that all stacks' routes
// share. These names have no dot, and the stacks' names have one.
const (
	challengeName = "acme-challenge"
	redirectName  = "redirect-to-https"
)

// challengePriority puts the router of the ACME challenges before every
// stack's router, because Traefik's default priority is the rule's length.
const challengePriority = 1_000_000

// RenderDynamic writes the routes for the file provider. Each route gets a
// service with its targets, named "<project>.<service>", and routers named
// "<entrypoint>.<project>.<service>". Projects and services have no dot, so
// the names never clash. A route whose domain has one of the certificates
// gets a router on websecure and a router on web that redirects to it.
// Without a certificate, the route gets only a plain router on web, so
// nobody sees Traefik's self-signed default certificate. A route of a stack
// on this host also has a router on the internal entrypoint, to which other
// hosts' Traefiks forward its requests; TLS has already ended there.
//
// The ACME challenges go to the agent on every host, whatever the domain,
// and also on the internal entrypoint: the leader checks a challenge there
// on the ingress host before the CA asks for it (controller.checkAddress).
func RenderDynamic(routes []Route, certificates []CertificateFiles) ([]byte, error) {
	config := dynamicConfig{HTTP: httpConfig{
		Routers: map[string]router{
			challengeName: {
				EntryPoints: []string{"web", "internal"}, Rule: "PathPrefix(`" + ChallengePath + "`)",
				Priority: challengePriority, Service: challengeName,
			},
		},
		Middlewares: map[string]middleware{
			redirectName: {RedirectScheme: redirectScheme{Scheme: "https", Permanent: true}},
		},
		Services: map[string]service{
			challengeName: {LoadBalancer: loadBalancer{Servers: []server{
				{URL: "http://" + ChallengeAddress},
			}}},
		},
	}}
	certified := map[string]bool{}
	for _, certificate := range certificates {
		if config.TLS == nil {
			config.TLS = &tlsConfig{}
		}
		config.TLS.Certificates = append(config.TLS.Certificates, tlsCertificate{
			CertFile: path.Join(certificatesDir, certificate.Chain), KeyFile: path.Join(certificatesDir, certificate.Key),
		})
		certified[certificate.Domain] = true
	}
	for _, route := range routes {
		name := route.Project + "." + route.Service
		if certified[strings.ToLower(route.Host)] {
			config.HTTP.Routers["websecure."+name] = router{
				EntryPoints: []string{"websecure"}, Rule: rule(route), Service: name, TLS: &routerTLS{},
			}
			config.HTTP.Routers["web."+name] = router{
				EntryPoints: []string{"web"}, Rule: rule(route), Middlewares: []string{redirectName}, Service: name,
			}
		} else {
			config.HTTP.Routers["web."+name] = router{EntryPoints: []string{"web"}, Rule: rule(route), Service: name}
		}
		if !route.Forwarded {
			config.HTTP.Routers["internal."+name] = router{
				EntryPoints: []string{"internal"}, Rule: rule(route), Service: name,
			}
		}
		servers := []server{}
		for _, target := range slices.Sorted(slices.Values(route.Targets)) {
			servers = append(servers, server{URL: "http://" + target})
		}
		config.HTTP.Services[name] = service{LoadBalancer: loadBalancer{Servers: servers}}
	}
	return marshal("Traefik's routes", config)
}

// rule matches the route's host and, unless the path is "/" or empty, the
// path and everything below it. stackspec.Check made sure that neither
// contains a backtick.
func rule(route Route) string {
	matcher := "Host(`" + strings.ToLower(route.Host) + "`)"
	if route.Path != "" && route.Path != "/" {
		matcher += " && PathPrefix(`" + route.Path + "`)"
	}
	return matcher
}

// marshal writes YAML with an indent of two, like the other files that
// clowder renders.
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

// Target is an address and port in the form Traefik takes them, for a
// route's target or an entrypoint's address.
func Target(address netip.Addr, port uint16) string {
	return netip.AddrPortFrom(address, port).String()
}
