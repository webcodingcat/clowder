// Package compose reads a Compose file with clowder's x-clowder fields, checks
// what clowder can run, and renders the file that docker compose gets on a
// host. It is portable: the CLI checks a stack before sending it.
package compose

import (
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/webcodingcat/clowder/internal/clusterdir"
)

// Stack is a Compose project that passed clowder's checks, with the
// x-clowder fields read into typed values.
type Stack struct {
	Name     string
	Priority clusterdir.Priority
	// Exports are services of this stack that other stacks may reach.
	Exports []string
	// Imports are services of other stacks ("stack/service") this one reaches.
	Imports []string
	// SecretsFile is the SOPS file with the stack's secrets, relative to its
	// first file. Only Load has it: it is the user's path, so Source leaves
	// it out.
	SecretsFile string
	Services    map[string]Service
	Volumes     map[string]Volume
	// Project is the Compose model without the x-clowder fields; Render
	// starts from it.
	Project *types.Project
	// source is the model with them, for Source.
	source *types.Project
}

// Service holds what clowder needs to know about one service.
type Service struct {
	Name string
	// Priority is the service's own; the zero value means the stack's.
	Priority clusterdir.Priority
	// HTTP is the route through Traefik, if the service has one.
	HTTP *HTTPRoute
	// Restart is the restart rule from Compose; the agent applies it itself.
	Restart          string
	RestartUnhealthy bool
	DependsOn        []string
	// MemoryReservation and CPUReservation come from deploy.resources; zero
	// means the cluster's default.
	MemoryReservation int64
	CPUReservation    float64
	Ports             []PublishedPort
	// Privileges are what the service asks for beyond a normal container
	// (privileged, a capability that reaches beyond the container, a
	// security_opt); only stacks the cluster allows them may have them.
	Privileges []string
	// Secrets appear in the container as files /run/secrets/<name>;
	// SecretEnv sets variables to secrets (variable name -> secret name).
	Secrets   []string
	SecretEnv map[string]string
}

// SecretNames are the names of the secrets the stack's services ask for,
// sorted, each once.
func (s *Stack) SecretNames() []string {
	asked := map[string]bool{}
	for _, service := range s.Services {
		for _, name := range service.Secrets {
			asked[name] = true
		}
		for _, name := range service.SecretEnv {
			asked[name] = true
		}
	}
	return slices.Sorted(maps.Keys(asked))
}

// PublishedPorts are the keys of the ports the stack publishes, sorted:
// the cluster refuses a port another stack has.
func (s *Stack) PublishedPorts() []string {
	var keys []string
	for _, service := range s.Services {
		for _, port := range service.Ports {
			keys = append(keys, port.Key())
		}
	}
	slices.Sort(keys)
	return keys
}

// Routes are the keys of the stack's HTTP routes, sorted: the cluster
// refuses a route another stack has.
func (s *Stack) Routes() []string {
	var keys []string
	for _, service := range s.Services {
		if service.HTTP != nil {
			keys = append(keys, service.HTTP.Key())
		}
	}
	slices.Sort(keys)
	return keys
}

// HTTPRoute sends requests for Host (and Path) to Port of the service.
type HTTPRoute struct {
	Host string
	Port uint16
	Path string
}

// Key is the route as the cluster tracks it: the host in lowercase (DNS
// ignores case) and the path, "/" for none; "shop.example.com/api".
func (r HTTPRoute) Key() string {
	path := r.Path
	if path == "" {
		path = "/"
	}
	return strings.ToLower(r.Host) + path
}

// PublishedPort is a TCP or UDP port published on the Floating IP.
type PublishedPort struct {
	Protocol  string
	Published uint16
	Target    uint32
}

// Key is the port as the cluster tracks it: "udp/9987".
func (p PublishedPort) Key() string {
	return p.Protocol + "/" + strconv.Itoa(int(p.Published))
}

// Volume holds the clowder fields of a named volume.
type Volume struct {
	Name string
	// Class is a storage class from cluster.yaml; whether the cluster has it is
	// checked when the stack is applied.
	Class    string
	Size     int64
	Replicas int
	Backup   *Backup
}

// Backup is the backup plan of a volume.
type Backup struct {
	Schedule string
	Keep     int
	Pre      *BackupHook
}

// BackupHook runs a command in a service before the snapshot.
type BackupHook struct {
	Service string
	Command []string
}
