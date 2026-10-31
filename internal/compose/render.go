// Package compose renders a stack's spec (stackspec) into the Compose file
// that docker compose gets on its host: Compose is how a host runs a stack,
// not what users write.
package compose

import (
	"bytes"
	"fmt"
	"maps"
	"net/netip"
	"path"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/stackspec"
	"github.com/webcodingcat/clowder/internal/units"
)

// The Compose file Render writes, as its own types: only what a spec can
// say ends up in it, and the fields come in a fixed order.
type (
	composeFile struct {
		Name     string                    `yaml:"name"`
		Services map[string]composeService `yaml:"services"`
		Networks map[string]composeNetwork `yaml:"networks"`
		Volumes  map[string]composeVolume  `yaml:"volumes,omitempty"`
		Secrets  map[string]composeSecret  `yaml:"secrets,omitempty"`
	}
	composeService struct {
		ContainerName   string                       `yaml:"container_name"`
		Image           string                       `yaml:"image"`
		Command         []string                     `yaml:"command,omitempty"`
		Entrypoint      []string                     `yaml:"entrypoint,omitempty"`
		WorkingDir      string                       `yaml:"working_dir,omitempty"`
		User            string                       `yaml:"user,omitempty"`
		Environment     map[string]string            `yaml:"environment,omitempty"`
		Secrets         []string                     `yaml:"secrets,omitempty"`
		Volumes         []composeMount               `yaml:"volumes,omitempty"`
		Ports           []composePort                `yaml:"ports,omitempty"`
		DependsOn       map[string]composeDependency `yaml:"depends_on,omitempty"`
		Healthcheck     *composeHealthcheck          `yaml:"healthcheck,omitempty"`
		Restart         string                       `yaml:"restart"`
		DNS             []string                     `yaml:"dns"`
		Labels          map[string]string            `yaml:"labels"`
		Deploy          *composeDeploy               `yaml:"deploy,omitempty"`
		StopGracePeriod string                       `yaml:"stop_grace_period,omitempty"`
		StopSignal      string                       `yaml:"stop_signal,omitempty"`
		ReadOnly        bool                         `yaml:"read_only,omitempty"`
		Init            bool                         `yaml:"init,omitempty"`
		ShmSize         int64                        `yaml:"shm_size,omitempty"`
		CapAdd          []string                     `yaml:"cap_add,omitempty"`
		CapDrop         []string                     `yaml:"cap_drop,omitempty"`
		Privileged      bool                         `yaml:"privileged,omitempty"`
		SecurityOpt     []string                     `yaml:"security_opt,omitempty"`
		Networks        []string                     `yaml:"networks"`
	}
	composeMount struct {
		Type     string        `yaml:"type"`
		Source   string        `yaml:"source,omitempty"`
		Target   string        `yaml:"target"`
		ReadOnly bool          `yaml:"read_only,omitempty"`
		Tmpfs    *composeTmpfs `yaml:"tmpfs,omitempty"`
	}
	composeTmpfs struct {
		Size int64 `yaml:"size"`
	}
	composePort struct {
		Target uint16 `yaml:"target"`
		// Published is a string: Compose's schema takes a range ("80-81") there
		Published string `yaml:"published"`
		HostIP    string `yaml:"host_ip"`
		Protocol  string `yaml:"protocol"`
	}
	composeDependency struct {
		Condition string `yaml:"condition"`
	}
	composeHealthcheck struct {
		Test        []string `yaml:"test"`
		Interval    string   `yaml:"interval,omitempty"`
		Timeout     string   `yaml:"timeout,omitempty"`
		StartPeriod string   `yaml:"start_period,omitempty"`
		Retries     int      `yaml:"retries,omitempty"`
	}
	composeDeploy struct {
		Resources composeResources `yaml:"resources"`
	}
	composeResources struct {
		Reservations *composeAmount `yaml:"reservations,omitempty"`
		Limits       *composeAmount `yaml:"limits,omitempty"`
	}
	// composeAmount's values are strings: Compose's schema wants them so
	composeAmount struct {
		Memory string `yaml:"memory,omitempty"`
		CPUs   string `yaml:"cpus,omitempty"`
	}
	composeNetwork struct {
		Name       string            `yaml:"name"`
		DriverOpts map[string]string `yaml:"driver_opts"`
	}
	composeVolume struct {
		Name       string            `yaml:"name"`
		Driver     string            `yaml:"driver"`
		DriverOpts map[string]string `yaml:"driver_opts"`
	}
	composeSecret struct {
		File string `yaml:"file"`
	}
)

// stackNetwork is the key of a stack's one network in its Compose file.
const stackNetwork = "net"

// Target is what the agent of a host knows when it renders a stack.
type Target struct {
	// Namespace is the stack's namespace: with the stack's name it makes the
	// names on the host (names.Project, names.BridgeName).
	Namespace string
	// FloatingIP and NodeAddress: published ports bind to both, so the
	// Floating IP reaches the container when this host is the ingress host
	// and the node address when another host forwards to it.
	FloatingIP  netip.Addr
	NodeAddress netip.Addr
	// Resolver is the agent's DNS resolver for the containers.
	Resolver netip.Addr
	// Volumes per volume name: the directory where its file system is
	// mounted, and its name on the host.
	Volumes map[string]MountedVolume
	// SecretsDir holds a file per secret the services ask for as a file.
	SecretsDir string
	// SecretLabels per service that asks for secret files: a keyed digest
	// of them (LabelSecrets). A changed file keeps its path, so without it
	// Compose would not recreate the container; with a key, the label tells
	// nothing about the values to whoever reads it.
	SecretLabels map[string]string
}

// MountedVolume is a volume as it is mounted on the host.
type MountedVolume struct {
	Dir string
	// Name is the Docker volume's, the copy's LV (names.VolumeName): it has
	// the data generation, as Docker cannot change the options of an
	// existing volume and a restore or class move makes a new one.
	Name string
}

// Labels that the agent puts on containers: the stack's Compose project
// (names.Project) on every one, the digest of its secret files on those that
// ask for some. Nothing else, so an apply recreates only the containers
// whose configuration or secret files changed.
const (
	LabelStack   = "clowder.stack"
	LabelSecrets = "clowder.secrets"
)

// Render returns the Compose file that docker compose gets on the host for
// a stack; the caller has checked the spec (stackspec.Check), Render
// normalizes it. Docker never restarts a container itself (restart "no"):
// after a reboot nothing may start before the agent has looked at the
// cluster, and the agent applies the stack's restart rules instead. Every
// name on the host comes from names; a $ in a value is meant as it stands.
// HTTP routes are not in the Compose file: Traefik routes them (ingress).
func Render(stackName string, spec stackspec.Spec, target Target) ([]byte, error) {
	if err := checkTarget(stackName, target); err != nil {
		return nil, err
	}
	spec = stackspec.Normalize(spec)
	project := names.Project(target.Namespace, stackName)
	file := composeFile{
		Name: project, Services: map[string]composeService{},
		Networks: map[string]composeNetwork{stackNetwork: {
			Name: names.StackNetwork(target.Namespace, stackName),
			DriverOpts: map[string]string{
				// a fixed name, so nft can tell which stack a packet comes from
				"com.docker.network.bridge.name": names.BridgeName(names.ID(target.Namespace, stackName)),
			},
		}},
		Volumes: map[string]composeVolume{}, Secrets: map[string]composeSecret{},
	}
	for _, name := range slices.Sorted(maps.Keys(spec.Services)) {
		service, err := renderService(stackName, project, name, spec, target)
		if err != nil {
			return nil, err
		}
		file.Services[name] = service
		for _, secret := range spec.Services[name].Secrets {
			file.Secrets[secret] = composeSecret{File: path.Join(target.SecretsDir, secret)}
		}
		for _, mount := range spec.Services[name].Volumes {
			mounted, ok := target.Volumes[mount.Volume]
			if !ok || mounted.Name == "" {
				return nil, fmt.Errorf("render stack %s: volume %s is not mounted", stackName, mount.Volume)
			}
			// data/ exists only while the file system is mounted, so a
			// container cannot start on the host's root file system
			file.Volumes[mount.Volume] = composeVolume{
				Name: mounted.Name, Driver: "local",
				DriverOpts: map[string]string{"type": "none", "o": "bind", "device": path.Join(mounted.Dir, "data")},
			}
		}
	}
	marshaled, err := yaml.Marshal(file)
	if err != nil {
		return nil, fmt.Errorf("render stack %s: %w", stackName, err)
	}
	rendered, err := escapeDollars(marshaled)
	if err != nil {
		return nil, fmt.Errorf("render stack %s: %w", stackName, err)
	}
	return rendered, nil
}

// checkTarget refuses a target that lacks what every stack needs.
func checkTarget(stackName string, target Target) error {
	for _, required := range []struct {
		what    string
		address netip.Addr
	}{{"floating IP", target.FloatingIP}, {"node address", target.NodeAddress}, {"resolver", target.Resolver}} {
		if !required.address.IsValid() {
			return fmt.Errorf("render stack %s: the target has no %s", stackName, required.what)
		}
	}
	if !names.ValidNamespace(target.Namespace) {
		return fmt.Errorf("render stack %s: the target's namespace %q is not valid", stackName, target.Namespace)
	}
	return nil
}

// renderService renders one service of a spec.
func renderService(stackName, project, name string, spec stackspec.Spec, target Target) (composeService, error) {
	service := spec.Services[name]
	rendered := composeService{
		ContainerName:   names.Container(target.Namespace, stackName, name),
		Image:           service.Image,
		Command:         service.Command,
		Entrypoint:      service.Entrypoint,
		WorkingDir:      service.WorkingDir,
		User:            service.User,
		Environment:     service.Env,
		Secrets:         service.Secrets,
		Restart:         "no",
		DNS:             []string{target.Resolver.String()},
		Labels:          map[string]string{LabelStack: project},
		StopGracePeriod: service.StopGracePeriod,
		StopSignal:      service.StopSignal,
		ReadOnly:        service.ReadOnly,
		Init:            service.Init,
		CapAdd:          service.CapAdd,
		CapDrop:         service.CapDrop,
		Privileged:      service.Privileged,
		SecurityOpt:     service.SecurityOpt,
		Networks:        []string{stackNetwork},
	}
	if len(service.Secrets) > 0 {
		if target.SecretsDir == "" {
			return rendered, fmt.Errorf("render stack %s: the target has no directory for secrets", stackName)
		}
		if target.SecretLabels[name] == "" {
			return rendered, fmt.Errorf("render stack %s: no label for the secrets of service %s", stackName, name)
		}
		rendered.Labels[LabelSecrets] = target.SecretLabels[name]
	}
	var err error
	if rendered.Volumes, err = renderMounts(service); err != nil {
		return rendered, fmt.Errorf("render stack %s: service %s: %w", stackName, name, err)
	}
	for _, port := range service.Ports {
		for _, address := range []netip.Addr{target.FloatingIP, target.NodeAddress} {
			rendered.Ports = append(rendered.Ports, composePort{
				Target: port.Port, Published: strconv.Itoa(int(port.Published)), HostIP: address.String(),
				Protocol: port.Protocol,
			})
		}
	}
	for _, dependency := range service.DependsOn {
		condition := "service_started"
		if spec.Services[dependency].Healthcheck != nil {
			condition = "service_healthy"
		}
		if rendered.DependsOn == nil {
			rendered.DependsOn = map[string]composeDependency{}
		}
		rendered.DependsOn[dependency] = composeDependency{Condition: condition}
	}
	if check := service.Healthcheck; check != nil {
		rendered.Healthcheck = &composeHealthcheck{
			Test: append([]string{"CMD"}, check.Command...), Interval: check.Interval, Timeout: check.Timeout,
			StartPeriod: check.StartPeriod, Retries: check.Retries,
		}
	}
	if rendered.Deploy, err = renderResources(service.Resources); err != nil {
		return rendered, fmt.Errorf("render stack %s: service %s: %w", stackName, name, err)
	}
	if service.ShmSize != "" {
		if rendered.ShmSize, err = units.ParseSize(service.ShmSize); err != nil {
			return rendered, fmt.Errorf("render stack %s: service %s: %w", stackName, name, err)
		}
	}
	return rendered, nil
}

// renderMounts turns a service's volumes, tmpfs and localtime into
// Compose's mounts, in this order.
func renderMounts(service stackspec.Service) ([]composeMount, error) {
	var mounts []composeMount
	for _, mount := range service.Volumes {
		mounts = append(mounts, composeMount{
			Type: "volume", Source: mount.Volume, Target: mount.Path, ReadOnly: mount.ReadOnly,
		})
	}
	for _, tmpfs := range service.Tmpfs {
		mount := composeMount{Type: "tmpfs", Target: tmpfs.Path}
		if tmpfs.Size != "" {
			size, err := units.ParseSize(tmpfs.Size)
			if err != nil {
				return nil, err
			}
			mount.Tmpfs = &composeTmpfs{Size: size}
		}
		mounts = append(mounts, mount)
	}
	if service.Localtime {
		mounts = append(mounts, composeMount{
			Type: "bind", Source: "/etc/localtime", Target: "/etc/localtime", ReadOnly: true,
		})
	}
	return mounts, nil
}

// renderResources turns reservations and limits into Compose's deploy
// block; nil without either.
func renderResources(resources stackspec.Resources) (*composeDeploy, error) {
	if resources.Reserve == nil && resources.Limit == nil {
		return nil, nil
	}
	deploy := &composeDeploy{}
	var err error
	if deploy.Resources.Reservations, err = renderAmount(resources.Reserve); err != nil {
		return nil, err
	}
	if deploy.Resources.Limits, err = renderAmount(resources.Limit); err != nil {
		return nil, err
	}
	return deploy, nil
}

func renderAmount(amount *stackspec.Amount) (*composeAmount, error) {
	if amount == nil {
		return nil, nil
	}
	rendered := &composeAmount{}
	if amount.Memory != "" {
		memory, err := units.ParseSize(amount.Memory)
		if err != nil {
			return nil, err
		}
		rendered.Memory = strconv.FormatInt(memory, 10)
	}
	if amount.CPUs > 0 {
		rendered.CPUs = strconv.FormatFloat(amount.CPUs, 'f', -1, 64)
	}
	return rendered, nil
}

// RenderSecretEnv returns the second Compose file a stack comes up with:
// only the variables its services set to secrets (secretEnv), from values
// (secret name -> value). The agent keeps it on tmpfs, apart from the
// rendered file on disk. Nil if the stack sets none.
func RenderSecretEnv(stackName string, spec stackspec.Spec, values map[string]string) ([]byte, error) {
	file := secretEnvFile{Services: map[string]secretEnvService{}}
	for name, service := range spec.Services {
		if len(service.SecretEnv) == 0 {
			continue
		}
		environment := map[string]string{}
		for variable, secret := range service.SecretEnv {
			value, ok := values[secret]
			if !ok {
				return nil, fmt.Errorf("render the secrets of stack %s: no value for %s", stackName, secret)
			}
			environment[variable] = value
		}
		file.Services[name] = secretEnvService{Environment: environment}
	}
	if len(file.Services) == 0 {
		return nil, nil
	}
	marshaled, err := yaml.Marshal(file)
	if err != nil {
		return nil, fmt.Errorf("render the secrets of stack %s: %w", stackName, err)
	}
	return escapeDollars(marshaled)
}

// secretEnvFile is the second Compose file: the secret variables only,
// which Compose merges into the services of the first.
type secretEnvFile struct {
	Services map[string]secretEnvService `yaml:"services"`
}

type secretEnvService struct {
	Environment map[string]string `yaml:"environment"`
}

// escapeDollars writes every $ in a value of a Compose file as $$: docker
// compose on the host interpolates the file it gets, and a $ in a value of
// a spec is meant as it stands. Keys are never interpolated.
func escapeDollars(file []byte) ([]byte, error) {
	var document yaml.Node
	if err := yaml.Unmarshal(file, &document); err != nil {
		return nil, fmt.Errorf("escape the $ signs: %w", err)
	}
	escapeValues(&document)
	var escaped bytes.Buffer
	encoder := yaml.NewEncoder(&escaped)
	encoder.SetIndent(2)
	if err := encoder.Encode(&document); err != nil {
		return nil, fmt.Errorf("escape the $ signs: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return nil, fmt.Errorf("escape the $ signs: %w", err)
	}
	return escaped.Bytes(), nil
}

func escapeValues(node *yaml.Node) {
	switch node.Kind {
	case yaml.ScalarNode:
		node.Value = strings.ReplaceAll(node.Value, "$", "$$")
	case yaml.MappingNode:
		for i := 1; i < len(node.Content); i += 2 { // keys at even places
			escapeValues(node.Content[i])
		}
	default:
		for _, child := range node.Content {
			escapeValues(child)
		}
	}
}
