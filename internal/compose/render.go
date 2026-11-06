// Package compose renders a stack's spec (stackspec) into the Compose file
// that docker compose gets on the stack's host. Compose is the way a host
// runs a stack; users do not write it.
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
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/stackspec"
	"github.com/webcodingcat/clowder/internal/units"
)

// The Compose file that Render writes, as its own types. The file holds
// only what a spec can say, and its fields come in a fixed order.
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
		Secrets         []composeServiceSecret       `yaml:"secrets,omitempty"`
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
		// Published is a string, because Compose's schema allows a range
		// ("80-81") here
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
	// composeAmount's values are strings, because Compose's schema wants
	// strings
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
	composeServiceSecret struct {
		Source string `yaml:"source"`
		Target string `yaml:"target"`
	}
)

// secretsPath is where a container finds its secret files: a directory per
// secret and a file per key, as Kubernetes mounts a secret.
const secretsPath = "/run/secrets"

// secretFile returns the name of the Compose secret for one key of a
// secret. No secret's name has "_", so "_" can separate the two.
func secretFile(secret, key string) string {
	return secret + "_" + key
}

// stackNetwork is the key of a stack's one network in its Compose file.
const stackNetwork = "net"

// Target is what a host's agent knows when it renders a stack.
type Target struct {
	// Namespace is the stack's namespace. Together with the stack's name,
	// it makes the names on the host (names.Project, names.BridgeName).
	Namespace string
	// FloatingIP and NodeAddress are where published ports bind. Traffic to
	// the Floating IP reaches the container when this host is the ingress
	// host. Traffic to the node address reaches it when another host
	// forwards the traffic here.
	FloatingIP  netip.Addr
	NodeAddress netip.Addr
	// Resolver is the agent's DNS resolver for the containers.
	Resolver netip.Addr
	// Volumes maps each volume's name to the directory where its file
	// system is mounted and to its name on the host.
	Volumes map[string]MountedVolume
	// SecretsDir holds a directory for each secret that the services use
	// as files, with a file per key.
	SecretsDir string
	// SecretKeys are the keys of each secret that the services use as
	// files.
	SecretKeys map[string][]string
	// SecretLabels holds a keyed digest of the secret files of each
	// service that uses some (LabelSecrets). A changed file keeps its
	// path, so without the label Compose would not recreate the container.
	// The digest is keyed, so the label reveals nothing about the values
	// to anyone who reads it.
	SecretLabels map[string]string
	// Restarted holds the time of the restart for each service that a
	// user restarted (LabelRestarted). A new time changes the container's
	// configuration, so Compose recreates the container.
	Restarted map[string]time.Time
}

// MountedVolume is a volume as it is mounted on the host.
type MountedVolume struct {
	Dir string
	// Name is the Docker volume's name, which is the copy's LV
	// (names.VolumeName). It includes the data generation, because Docker
	// cannot change the options of an existing volume, and a new data
	// generation needs a new Docker volume.
	Name string
}

// The labels that the agent puts on containers. LabelStack holds the
// stack's Compose project (names.Project) on every container. LabelSecrets
// holds the digest of the secret files on the containers that use some.
// LabelRestarted holds the time of the last restart on the containers that
// a user restarted. There are no other labels. So an apply recreates only
// the containers whose configuration or secret files changed, and a
// restart recreates only the services that it names.
const (
	LabelStack     = "clowder.stack"
	LabelSecrets   = "clowder.secrets"
	LabelRestarted = "clowder.restarted"
)

// RestartedLabel returns the value of LabelRestarted for a restart at the
// time at. The agent compares it with the label of a running container.
func RestartedLabel(at time.Time) string {
	return at.UTC().Format(time.RFC3339Nano)
}

// Render returns the Compose file that docker compose gets on the host for
// a stack. The caller must have checked the spec (stackspec.Check), and
// Render normalizes it.
//
// Docker never restarts a container itself (restart "no"). After a
// reboot, nothing may start before the agent has looked at the cluster,
// and the agent applies the stack's restart rules instead. Every name on
// the host comes from names. A $ in a value is meant literally. HTTP
// routes are not in the Compose file, because Traefik routes them
// (ingress).
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
			for _, key := range target.SecretKeys[secret] {
				file.Secrets[secretFile(secret, key)] = composeSecret{File: path.Join(target.SecretsDir, secret, key)}
			}
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
		for _, secret := range service.Secrets {
			keys := target.SecretKeys[secret]
			if len(keys) == 0 {
				return rendered, fmt.Errorf("render stack %s: no keys for secret %s", stackName, secret)
			}
			for _, key := range slices.Sorted(slices.Values(keys)) {
				rendered.Secrets = append(rendered.Secrets, composeServiceSecret{
					Source: secretFile(secret, key), Target: path.Join(secretsPath, secret, key),
				})
			}
		}
	}
	if at, ok := target.Restarted[name]; ok {
		rendered.Labels[LabelRestarted] = RestartedLabel(at)
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
// block. It returns nil if there are neither.
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

// RenderSecretEnv returns the second Compose file of a stack. It holds
// only the variables that the services set to values of secrets
// (secretEnv), with the values taken from values. The agent keeps this
// file on tmpfs, apart from the rendered file on disk. RenderSecretEnv
// returns nil if the stack sets no such variables.
func RenderSecretEnv(stackName string, spec stackspec.Spec, values map[names.SecretKeyRef]string) ([]byte, error) {
	file := secretEnvFile{Services: map[string]secretEnvService{}}
	for name, service := range spec.Services {
		if len(service.SecretEnv) == 0 {
			continue
		}
		environment := map[string]string{}
		for variable, ref := range service.SecretEnv {
			value, ok := values[ref]
			if !ok {
				return nil, fmt.Errorf("render the secrets of stack %s: no value for %s", stackName, ref)
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

// secretEnvFile is the second Compose file. It holds only the secret
// variables, which Compose merges into the services of the first file.
type secretEnvFile struct {
	Services map[string]secretEnvService `yaml:"services"`
}

type secretEnvService struct {
	Environment map[string]string `yaml:"environment"`
}

// escapeDollars writes every $ in a value of a Compose file as $$. docker
// compose on the host interpolates the file, but a $ in a spec's value is
// meant literally. Compose never interpolates keys.
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
