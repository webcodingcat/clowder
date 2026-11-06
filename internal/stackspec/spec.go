// Package stackspec is a stack as clowder describes it: its own schema,
// not a Compose file. Compose is only how the agent runs a stack on a
// host; the agent renders the Compose file from the spec (compose.Render).
// A field that the schema does not have is an error, so only what clowder
// knows runs. The package does no I/O, because the CLI, the API and the
// state share it.
package stackspec

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/webcodingcat/clowder/internal/names"
)

// Spec is a stack: its services, and the priority they have unless they
// name their own.
type Spec struct {
	// Priority is a number from 0 to 1000 or a category of the
	// configuration. Empty means the cluster's default, which the scheduler
	// applies when it places, so a changed configuration changes no stored
	// spec.
	Priority string             `yaml:"priority,omitempty" json:"priority,omitempty"`
	Services map[string]Service `yaml:"services" json:"services"`
}

// Service is one container of a stack, by its name in Spec.Services. The
// order of a list matters where the comment says so. The other lists are
// sets, which Normalize sorts.
type Service struct {
	Image string `yaml:"image" json:"image"`
	// Command and Entrypoint keep their order.
	Command    []string `yaml:"command,omitempty" json:"command,omitempty"`
	Entrypoint []string `yaml:"entrypoint,omitempty" json:"entrypoint,omitempty"`
	WorkingDir string   `yaml:"workingDir,omitempty" json:"workingDir,omitempty"`
	User       string   `yaml:"user,omitempty" json:"user,omitempty"`
	// Env takes any of YAML's scalars and keeps the text as written
	// (8080, 1.0, yes): the value is a string to the container anyway.
	Env map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
	// SecretEnv sets variables to values of secrets of the namespace:
	// variable -> secret and key, as Kubernetes' secretKeyRef.
	SecretEnv map[string]names.SecretKeyRef `yaml:"secretEnv,omitempty" json:"secretEnv,omitempty"`
	// Secrets are secrets of the namespace, each as a directory
	// /run/secrets/<name> with a file per key, as Kubernetes mounts a
	// secret.
	Secrets []string `yaml:"secrets,omitempty" json:"secrets,omitempty"`
	// Volumes are a set by path. Docker mounts the outer path before the
	// inner one, in whatever order they are written.
	Volumes []Mount `yaml:"volumes,omitempty" json:"volumes,omitempty"`
	Tmpfs   []Tmpfs `yaml:"tmpfs,omitempty" json:"tmpfs,omitempty"`
	// Localtime mounts the host's /etc/localtime read-only.
	Localtime bool   `yaml:"localtime,omitempty" json:"localtime,omitempty"`
	Ports     []Port `yaml:"ports,omitempty" json:"ports,omitempty"`
	HTTP      *HTTP  `yaml:"http,omitempty" json:"http,omitempty"`
	// DependsOn are services of the stack that this one starts after. For
	// a service with a healthcheck, it starts once that service is healthy.
	DependsOn   []string     `yaml:"dependsOn,omitempty" json:"dependsOn,omitempty"`
	Healthcheck *Healthcheck `yaml:"healthcheck,omitempty" json:"healthcheck,omitempty"`
	// Restart is always (the default), no, on-failure or on-failure:<n>.
	// The supervisor applies it; Docker never restarts a container itself.
	Restart string `yaml:"restart,omitempty" json:"restart,omitempty"`
	// RestartUnhealthy restarts a container that turns unhealthy (default
	// true; RestartsUnhealthy reads it).
	RestartUnhealthy *bool     `yaml:"restartUnhealthy,omitempty" json:"restartUnhealthy,omitempty"`
	Resources        Resources `yaml:"resources,omitempty" json:"resources,omitzero"`
	// StopGracePeriod is a duration (10s, 1m30s), StopSignal a signal's
	// name (SIGTERM).
	StopGracePeriod string `yaml:"stopGracePeriod,omitempty" json:"stopGracePeriod,omitempty"`
	StopSignal      string `yaml:"stopSignal,omitempty" json:"stopSignal,omitempty"`
	ReadOnly        bool   `yaml:"readOnly,omitempty" json:"readOnly,omitempty"`
	Init            bool   `yaml:"init,omitempty" json:"init,omitempty"`
	ShmSize         string `yaml:"shmSize,omitempty" json:"shmSize,omitempty"`
	// CapAdd beyond the harmless capabilities, Privileged, and SecurityOpt
	// beyond no-new-privileges reach outside the container (Privileges).
	CapAdd      []string `yaml:"capAdd,omitempty" json:"capAdd,omitempty"`
	CapDrop     []string `yaml:"capDrop,omitempty" json:"capDrop,omitempty"`
	Privileged  bool     `yaml:"privileged,omitempty" json:"privileged,omitempty"`
	SecurityOpt []string `yaml:"securityOpt,omitempty" json:"securityOpt,omitempty"`
	// Priority is the service's own priority; if empty, the stack's
	// applies.
	Priority string `yaml:"priority,omitempty" json:"priority,omitempty"`
}

// Mount mounts a volume of the namespace at an absolute path.
type Mount struct {
	Volume   string `yaml:"volume" json:"volume"`
	Path     string `yaml:"path" json:"path"`
	ReadOnly bool   `yaml:"readOnly,omitempty" json:"readOnly,omitempty"`
}

// Tmpfs is a file system in memory at an absolute path, of at most Size
// (512M) if set.
type Tmpfs struct {
	Path string `yaml:"path" json:"path"`
	Size string `yaml:"size,omitempty" json:"size,omitempty"`
}

// Port publishes a container's port on the cluster's Floating IP.
type Port struct {
	Port uint16 `yaml:"port" json:"port"`
	// Published is the port outside, by default the container's port.
	Published uint16 `yaml:"published,omitempty" json:"published,omitempty"`
	// Protocol is tcp (the default) or udp.
	Protocol string `yaml:"protocol,omitempty" json:"protocol,omitempty"`
}

// HTTP routes the requests for a host, and a path below it, to a port of
// the service through Traefik.
type HTTP struct {
	Host string `yaml:"host" json:"host"`
	Port uint16 `yaml:"port" json:"port"`
	Path string `yaml:"path,omitempty" json:"path,omitempty"`
}

// Healthcheck runs a command in the container (exec form; a shell is
// [sh, -c, "..."]).
type Healthcheck struct {
	Command     []string `yaml:"command" json:"command"`
	Interval    string   `yaml:"interval,omitempty" json:"interval,omitempty"`
	Timeout     string   `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	StartPeriod string   `yaml:"startPeriod,omitempty" json:"startPeriod,omitempty"`
	Retries     int      `yaml:"retries,omitempty" json:"retries,omitempty"`
}

// Resources are what a service reserves and its limits. The scheduler
// places by the reservations, or by the configuration's
// defaults.reservations if there are none.
type Resources struct {
	Reserve *Amount `yaml:"reserve,omitempty" json:"reserve,omitempty"`
	Limit   *Amount `yaml:"limit,omitempty" json:"limit,omitempty"`
}

// Amount is memory (512M) and CPUs (0.5).
type Amount struct {
	Memory string  `yaml:"memory,omitempty" json:"memory,omitempty"`
	CPUs   float64 `yaml:"cpus,omitempty" json:"cpus,omitempty"`
}

// Decode reads a spec as users write it, in YAML. A field the schema does
// not have is an error, not ignored.
//
// JSON is YAML too, so the API also uses Decode for a spec that the CLI
// sends as JSON. The CLI keeps each number as written
// (resource.ParseDocuments), so an env value of 1.0 arrives as "1.0". A
// number that JSON cannot write as it is arrives as YAML reads it (0x10 as
// "16"), and a boolean as true or false.
func Decode(data []byte) (Spec, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var spec Spec
	if err := decoder.Decode(&spec); err != nil {
		if errors.Is(err, io.EOF) {
			return Spec{}, errors.New("read the stack: it is empty")
		}
		return Spec{}, fmt.Errorf("read the stack: %w", err)
	}
	// a second document would be dropped silently
	var rest yaml.Node
	if err := decoder.Decode(&rest); !errors.Is(err, io.EOF) {
		return Spec{}, errors.New("read the stack: it holds more than one YAML document")
	}
	return spec, nil
}

// Problems lists everything wrong with a spec at once.
type Problems []string

func (p Problems) Error() string {
	return "the stack cannot run on clowder:\n  - " + strings.Join(p, "\n  - ")
}
