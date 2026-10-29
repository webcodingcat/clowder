package compose

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"

	"github.com/webcodingcat/clowder/internal/units"
)

// LoadOptions says where a stack's Compose files are.
type LoadOptions struct {
	// Name is the stack's name (the Compose project name).
	Name string
	// WorkingDir is where relative paths (env_file, extends) start.
	WorkingDir string
	// Files are the Compose files, later ones override earlier ones.
	Files []string
	// Environment is used for ${VARIABLE} interpolation.
	Environment map[string]string
}

// Problems lists everything clowder cannot run in a stack, so the user sees
// all of it at once.
type Problems []string

func (p Problems) Error() string {
	return "the stack cannot run on clowder:\n  - " + strings.Join(p, "\n  - ")
}

// Load reads the Compose files, checks them and returns the stack. A stack
// that clowder cannot run gives Problems.
func Load(ctx context.Context, options LoadOptions) (*Stack, error) {
	details := types.ConfigDetails{
		WorkingDir:  options.WorkingDir,
		Environment: options.Environment,
	}
	for _, file := range options.Files {
		details.ConfigFiles = append(details.ConfigFiles, types.ConfigFile{Filename: file})
	}
	project, err := loader.LoadWithContext(ctx, details, func(o *loader.Options) {
		o.SetProjectName(options.Name, true)
	})
	if err != nil {
		return nil, fmt.Errorf("read the compose files: %w", err)
	}
	return check(project)
}

// check turns a loaded project into a Stack, or lists its problems.
func check(project *types.Project) (*Stack, error) {
	stack := &Stack{
		Name:     project.Name,
		Services: map[string]Service{},
		Volumes:  map[string]Volume{},
	}
	var problems Problems
	report := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	var stackExtension rawStackExtension
	if err := decodeExtension(project.Extensions, &stackExtension); err != nil {
		report("stack: %v", err)
	}
	priority, err := parsePriority(stackExtension.Priority)
	if err != nil {
		report("stack: %v", err)
	}
	stack.Priority = priority
	stack.Exports = stackExtension.Exports
	stack.Imports = stackExtension.Imports

	for _, name := range slices.Sorted(maps.Keys(project.Volumes)) {
		volume, volumeProblems := checkVolume(name, project.Volumes[name])
		problems = append(problems, volumeProblems...)
		stack.Volumes[name] = volume
	}
	for _, name := range slices.Sorted(maps.Keys(project.Networks)) {
		network := project.Networks[name]
		if network.External {
			report("network %s: external networks are not supported", name)
		}
		if network.Driver != "" && network.Driver != "bridge" {
			report("network %s: driver %q is not supported (clowder uses bridges)", name, network.Driver)
		}
		if len(network.Ipam.Config) > 0 {
			report("network %s: ipam subnets are not supported (Docker's address pools assign them)", name)
		}
		if _, ok := network.Extensions[extensionKey]; ok {
			report("network %s: x-clowder is not used on networks", name)
		}
	}
	// a file named here lives on this machine, not on the host that runs the stack
	for _, name := range slices.Sorted(maps.Keys(project.Secrets)) {
		if project.Secrets[name].File != "" {
			report("secret %s: file is not supported; use environment", name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(project.Configs)) {
		if project.Configs[name].File != "" {
			report("config %s: file is not supported; use content or environment", name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(project.Services)) {
		service, serviceProblems := checkService(project.Services[name], project.Volumes)
		problems = append(problems, serviceProblems...)
		stack.Services[name] = service
	}
	problems = append(problems, checkPortsUnique(stack.Services)...)
	for _, export := range stack.Exports {
		if _, ok := stack.Services[export]; !ok {
			report("stack: exports %q, which is not a service of this stack", export)
		}
	}
	for _, imported := range stack.Imports {
		if stackName, service, ok := strings.Cut(imported, "/"); !ok || stackName == "" || service == "" {
			report("stack: import %q is not of the form stack/service", imported)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(stack.Volumes)) {
		volume := stack.Volumes[name]
		if volume.Backup != nil && volume.Backup.Pre != nil {
			if _, ok := stack.Services[volume.Backup.Pre.Service]; !ok {
				report("volume %s: the backup hook runs in %q, which is not a service of this stack",
					volume.Name, volume.Backup.Pre.Service)
			}
		}
	}
	if len(problems) > 0 {
		return nil, problems
	}
	stack.Project = withoutExtensions(project)
	return stack, nil
}

// checkVolume reads the clowder fields of a named volume.
func checkVolume(name string, config types.VolumeConfig) (Volume, Problems) {
	volume := Volume{Name: name}
	var problems Problems
	report := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf("volume %s: ", name)+fmt.Sprintf(format, args...))
	}
	if config.External {
		report("external volumes are not supported")
	}
	if config.Driver != "" || len(config.DriverOpts) > 0 {
		report("driver and driver_opts are set by clowder")
	}
	var raw rawVolumeExtension
	if err := decodeExtension(config.Extensions, &raw); err != nil {
		report("%v", err)
		return volume, problems
	}
	if raw.Class == "" {
		report("x-clowder.class is required (a storage class of the cluster, for example fast)")
	}
	volume.Class = raw.Class
	if raw.Size == "" {
		report("x-clowder.size is required (for example 10G)")
	} else if size, err := units.ParseSize(raw.Size); err != nil {
		report("%v", err)
	} else {
		volume.Size = size
	}
	switch {
	case raw.Replicas == nil:
		report("x-clowder.replicas is required (0 to 3)")
	case *raw.Replicas < 0 || *raw.Replicas > 3:
		report("x-clowder.replicas %d is not from 0 to 3", *raw.Replicas)
	default:
		volume.Replicas = *raw.Replicas
	}
	if raw.Backup != nil {
		if raw.Backup.Schedule == "" {
			report("x-clowder.backup.schedule is required when backup is set")
		}
		if raw.Replicas != nil && *raw.Replicas == 0 {
			report("a volume with replicas 0 is never backed up; remove x-clowder.backup")
		}
		if raw.Backup.Keep < 0 {
			report("x-clowder.backup.keep %d is negative", raw.Backup.Keep)
		}
		if hook := raw.Backup.Pre; hook != nil && (hook.Service == "" || len(hook.Command) == 0) {
			report("x-clowder.backup.pre needs service and command")
		}
		volume.Backup = &Backup{Schedule: raw.Backup.Schedule, Keep: raw.Backup.Keep}
		if raw.Backup.Pre != nil {
			volume.Backup.Pre = &BackupHook{Service: raw.Backup.Pre.Service, Command: raw.Backup.Pre.Command}
		}
	}
	return volume, problems
}

// checkService checks what clowder cannot run in a service and reads its
// clowder fields.
func checkService(config types.ServiceConfig, volumes types.Volumes) (Service, Problems) {
	service := Service{Name: config.Name, Restart: config.Restart, RestartUnhealthy: true}
	var problems Problems
	report := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf("service %s: ", config.Name)+fmt.Sprintf(format, args...))
	}

	if config.Build != nil {
		report("build is not supported; use a prebuilt image")
	}
	if config.NetworkMode == "host" || strings.HasPrefix(config.NetworkMode, "container:") ||
		strings.HasPrefix(config.NetworkMode, "service:") {
		report("network_mode %q is not supported", config.NetworkMode)
	}
	for _, namespace := range []struct{ field, value string }{
		{"pid", config.Pid},
		{"ipc", config.Ipc},
		{"uts", config.Uts},
		{"userns_mode", config.UserNSMode},
		{"cgroup", config.Cgroup},
	} {
		if namespace.value == "host" {
			report("%s: host is not supported", namespace.field)
		}
	}
	if config.ContainerName != "" {
		report("container_name is set by clowder")
	}
	if len(config.Devices) > 0 {
		report("devices are not supported")
	}
	if (config.Scale != nil && *config.Scale > 1) ||
		(config.Deploy != nil && config.Deploy.Replicas != nil && *config.Deploy.Replicas > 1) {
		report("more than one replica per service is not supported")
	}
	if !validRestart(config.Restart) {
		report("restart %q is not one of no, always, on-failure[:n], unless-stopped", config.Restart)
	}
	for _, mount := range config.Volumes {
		if problem := checkMount(mount, volumes); problem != "" {
			report("%s", problem)
		}
	}
	for _, port := range config.Ports {
		published, problem := checkPort(port)
		if problem != "" {
			report("%s", problem)
			continue
		}
		service.Ports = append(service.Ports, published)
	}
	service.DependsOn = slices.Sorted(maps.Keys(config.DependsOn))
	if config.Deploy != nil && config.Deploy.Resources.Reservations != nil {
		service.MemoryReservation = int64(config.Deploy.Resources.Reservations.MemoryBytes)
		// compose-go names the field NanoCPUs but keeps whole CPUs in it (0.5)
		service.CPUReservation = float64(config.Deploy.Resources.Reservations.NanoCPUs)
	}

	var raw rawServiceExtension
	if err := decodeExtension(config.Extensions, &raw); err != nil {
		report("%v", err)
		return service, problems
	}
	priority, err := parsePriority(raw.Priority)
	if err != nil {
		report("%v", err)
	}
	service.Priority = priority
	if raw.RestartUnhealthy != nil {
		service.RestartUnhealthy = *raw.RestartUnhealthy
	}
	if raw.HTTP != nil {
		if raw.HTTP.Host == "" || raw.HTTP.Port == 0 {
			report("x-clowder.http needs host and port")
		}
		service.HTTP = &HTTPRoute{Host: raw.HTTP.Host, Port: raw.HTTP.Port, Path: raw.HTTP.Path}
	}
	return service, problems
}

// validRestart tells whether a Compose restart rule is one the agent applies.
func validRestart(restart string) bool {
	switch {
	case restart == "", restart == "no", restart == "always", restart == "unless-stopped",
		restart == "on-failure":
		return true
	case strings.HasPrefix(restart, "on-failure:"):
		count, err := strconv.Atoi(strings.TrimPrefix(restart, "on-failure:"))
		return err == nil && count > 0
	default:
		return false
	}
}

// checkMount allows named volumes with x-clowder fields, tmpfs, and
// /etc/localtime read-only; it returns what is wrong, or "".
func checkMount(mount types.ServiceVolumeConfig, volumes types.Volumes) string {
	switch mount.Type {
	case types.VolumeTypeVolume:
		if mount.Source == "" {
			return fmt.Sprintf("the anonymous volume at %s needs a name and x-clowder fields", mount.Target)
		}
		if _, ok := volumes[mount.Source]; !ok {
			return fmt.Sprintf("volume %s is not declared under volumes", mount.Source)
		}
		return ""
	case types.VolumeTypeTmpfs:
		return ""
	case types.VolumeTypeBind:
		if mount.Source == "/etc/localtime" && mount.ReadOnly {
			return ""
		}
		return fmt.Sprintf("bind mount %s is not supported (only /etc/localtime read-only)", mount.Source)
	default:
		return fmt.Sprintf("mount type %s at %s is not supported", mount.Type, mount.Target)
	}
}

// checkPort reads a published port; ports without a single host port, with
// a host address, or on a reserved port are refused.
func checkPort(port types.ServicePortConfig) (PublishedPort, string) {
	published := PublishedPort{Protocol: port.Protocol, Target: port.Target}
	if published.Protocol == "" {
		published.Protocol = "tcp"
	}
	if published.Protocol != "tcp" && published.Protocol != "udp" {
		return published, fmt.Sprintf("port %d: protocol %s is not supported", port.Target, port.Protocol)
	}
	if port.HostIP != "" {
		return published, fmt.Sprintf("port %d: the host address is set by clowder", port.Target)
	}
	number, err := strconv.ParseUint(port.Published, 10, 16)
	if err != nil || number == 0 {
		return published, fmt.Sprintf("port %d: needs exactly one published port (got %q)", port.Target, port.Published)
	}
	published.Published = uint16(number)
	if ReservedPort(published.Published) {
		return published, fmt.Sprintf("port %d is reserved for clowder or the host", number)
	}
	return published, ""
}

// ReservedPort tells whether a port is used by the host or by clowder itself
// and may not be published by a stack.
func ReservedPort(port uint16) bool {
	switch {
	case port == 22, port == 53, port == 80, port == 443: // SSH, DNS, Traefik
		return true
	case port >= 7400 && port <= 7499: // Raft, agent API, metrics, ACME, Traefik inside
		return true
	case port >= 10000 && port <= 10999: // DRBD: 10000 + minor (state's maxMinor)
		return true
	case port >= 20000 && port <= 29999: // exported services
		return true
	case port >= 51800 && port <= 51831: // WireGuard: 51800 + host ID (state.MaxHosts)
		return true
	default:
		return false
	}
}

// withoutExtensions returns a copy of the project without x-clowder fields.
func withoutExtensions(project *types.Project) *types.Project {
	clean := *project
	clean.Extensions = without(project.Extensions)
	clean.Services = types.Services{}
	for name, service := range project.Services {
		service.Extensions = without(service.Extensions)
		clean.Services[name] = service
	}
	clean.Volumes = types.Volumes{}
	for name, volume := range project.Volumes {
		volume.Extensions = without(volume.Extensions)
		clean.Volumes[name] = volume
	}
	return &clean
}

// without returns extensions without x-clowder.
func without(extensions types.Extensions) types.Extensions {
	if _, ok := extensions[extensionKey]; !ok {
		return extensions
	}
	clean := maps.Clone(extensions)
	delete(clean, extensionKey)
	return clean
}

// checkPortsUnique: two services of a stack cannot publish the same port.
func checkPortsUnique(services map[string]Service) Problems {
	var problems Problems
	owners := map[string]string{}
	for _, name := range slices.Sorted(maps.Keys(services)) {
		for _, port := range services[name].Ports {
			key := fmt.Sprintf("%s/%d", port.Protocol, port.Published)
			if owner, taken := owners[key]; taken {
				problems = append(problems, fmt.Sprintf("service %s: port %s is published by %s already", name, key, owner))
				continue
			}
			owners[key] = name
		}
	}
	return problems
}
