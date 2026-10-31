package compose

import (
	"fmt"
	"maps"
	"net/netip"
	"path"
	"slices"
	"strconv"

	"go.yaml.in/yaml/v3"

	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/stackspec"
	"github.com/webcodingcat/clowder/internal/units"
)

// The Compose file RenderStack writes, as its own types: only what a spec
// can say ends up in it, and the fields come in a fixed order.
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

// RenderStack returns the Compose file that docker compose gets on the host
// for a stack with a spec of its own (stackspec); the caller has checked
// the spec (stackspec.Check), RenderStack normalizes it. As Render: Docker
// never restarts a container itself (restart "no"), the agent does; every
// name on the host comes from names; a $ in a value is meant as it stands.
// HTTP routes are not in the Compose file: Traefik routes them (ingress).
func RenderStack(stackName string, spec stackspec.Spec, target Target) ([]byte, error) {
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
				// the bridge of a stack's "default" network in Render: a stack
				// that moves to a spec keeps its bridge and nft rules
				"com.docker.network.bridge.name": names.BridgeName(names.ID(target.Namespace, stackName), "default"),
			},
		}},
		Volumes: map[string]composeVolume{}, Secrets: map[string]composeSecret{},
	}
	for _, name := range slices.Sorted(maps.Keys(spec.Services)) {
		service, err := renderSpecService(stackName, project, name, spec, target)
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
	rendered, err := EscapeDollars(marshaled)
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

// renderSpecService renders one service of a spec.
func renderSpecService(stackName, project, name string, spec stackspec.Spec, target Target) (composeService, error) {
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
		if target.SecretDigests[name] == "" {
			return rendered, fmt.Errorf("render stack %s: no digest of the secrets of service %s", stackName, name)
		}
		rendered.Labels[LabelSecrets] = target.SecretDigests[name]
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

// RenderStackSecretEnv returns the second Compose file of a stack with a
// spec of its own, as RenderSecretEnv: only the variables its services set
// to secrets, from values (secret name -> value). Nil if it sets none.
func RenderStackSecretEnv(stackName string, spec stackspec.Spec, values map[string]string) ([]byte, error) {
	secretEnv := map[string]map[string]string{}
	for name, service := range spec.Services {
		secretEnv[name] = service.SecretEnv
	}
	return renderSecretEnv(stackName, secretEnv, values)
}
