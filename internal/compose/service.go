package compose

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/webcodingcat/clowder/internal/names"
)

// routePath is an absolute URL path of the characters RFC 3986 allows.
var routePath = regexp.MustCompile(`^/[A-Za-z0-9._~!$&'()*+,;=:@%/-]*$`)

// checkService checks what clowder cannot run in a service and reads its
// clowder fields.
func checkService(config types.ServiceConfig, volumes types.Volumes) (Service, Problems) {
	service := Service{Name: config.Name, Restart: config.Restart, RestartUnhealthy: true}
	found := checkHostTies(config)
	for _, mount := range config.Volumes {
		if problem := checkMount(mount, volumes); problem != "" {
			found = append(found, problem)
		}
	}
	for _, port := range config.Ports {
		published, problem := checkPort(port)
		if problem != "" {
			found = append(found, problem)
			continue
		}
		service.Ports = append(service.Ports, published)
	}
	service.Privileges = privileges(config)
	service.DependsOn = slices.Sorted(maps.Keys(config.DependsOn))
	if config.Deploy != nil && config.Deploy.Resources.Reservations != nil {
		service.MemoryReservation = int64(config.Deploy.Resources.Reservations.MemoryBytes)
		// compose-go names the field NanoCPUs but keeps whole CPUs in it (0.5)
		service.CPUReservation = float64(config.Deploy.Resources.Reservations.NanoCPUs)
	}
	found = append(found, readServiceExtension(config, &service)...)

	var problems Problems
	for _, problem := range found {
		problems = append(problems, fmt.Sprintf("service %s: %s", config.Name, problem))
	}
	return service, problems
}

// checkHostTies refuses what ties a service to one host or shares the
// host's namespaces, and what clowder sets itself.
func checkHostTies(config types.ServiceConfig) []string {
	var problems []string
	report := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
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
	return problems
}

// readServiceExtension reads the service's x-clowder block into service.
func readServiceExtension(config types.ServiceConfig, service *Service) []string {
	var raw rawServiceExtension
	if err := decodeExtension(config.Extensions, &raw); err != nil {
		return []string{err.Error()}
	}
	var problems []string
	priority, err := parsePriority(raw.Priority)
	if err != nil {
		problems = append(problems, err.Error())
	}
	service.Priority = priority
	if raw.RestartUnhealthy != nil {
		service.RestartUnhealthy = *raw.RestartUnhealthy
	}
	if raw.HTTP != nil {
		if raw.HTTP.Host == "" || raw.HTTP.Port == 0 {
			problems = append(problems, "x-clowder.http needs host and port")
		}
		// both end up in a Traefik rule between backticks
		if raw.HTTP.Host != "" && !names.ValidDomain(raw.HTTP.Host) {
			problems = append(problems, fmt.Sprintf("x-clowder.http.host %q is not a domain name", raw.HTTP.Host))
		}
		if raw.HTTP.Path != "" && !routePath.MatchString(raw.HTTP.Path) {
			problems = append(problems, fmt.Sprintf(
				"x-clowder.http.path %q must start with / and hold only the characters of a URL path", raw.HTTP.Path))
		}
		service.HTTP = &HTTPRoute{Host: raw.HTTP.Host, Port: raw.HTTP.Port, Path: raw.HTTP.Path}
	}
	problems = append(problems, checkSecretRequests(config, raw)...)
	service.Secrets, service.SecretEnv = raw.Secrets, raw.SecretEnv
	return problems
}

// checkSecretRequests checks how a service asks for secrets: only through
// x-clowder, with valid names, each file once, no variable set twice.
func checkSecretRequests(config types.ServiceConfig, raw rawServiceExtension) []string {
	var problems []string
	report := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}
	if len(config.Secrets) > 0 {
		report("secrets: Compose's secrets are not supported; use x-clowder.secrets")
	}
	for _, name := range raw.Secrets {
		if !names.ValidSecret(name) {
			report("x-clowder.secrets: %q is not a secret name (lowercase letters, digits, - and _)", name)
		}
	}
	if len(slices.Compact(slices.Sorted(slices.Values(raw.Secrets)))) != len(raw.Secrets) {
		report("x-clowder.secrets: a secret is named twice")
	}
	for _, variable := range slices.Sorted(maps.Keys(raw.SecretEnv)) {
		switch {
		case !names.ValidVariable(variable):
			report("x-clowder.secret_env: %q is not a variable name", variable)
		case !names.ValidSecret(raw.SecretEnv[variable]):
			report("x-clowder.secret_env: %q is not a secret name", raw.SecretEnv[variable])
		}
		if _, ok := config.Environment[variable]; ok {
			report("x-clowder.secret_env: %s is set in environment as well", variable)
		}
	}
	return problems
}

// harmlessCapabilities act only inside the container's own namespaces (its
// network, its processes): Docker's default set, and a few that services
// commonly add. Any other capability reaches the host.
var harmlessCapabilities = map[string]bool{
	"AUDIT_WRITE": true, "CHOWN": true, "DAC_OVERRIDE": true, "FOWNER": true, "FSETID": true,
	"KILL": true, "MKNOD": true, "NET_BIND_SERVICE": true, "NET_RAW": true, "SETFCAP": true,
	"SETGID": true, "SETPCAP": true, "SETUID": true, "SYS_CHROOT": true,
	"NET_ADMIN": true, "NET_BROADCAST": true, "IPC_LOCK": true, "SYS_NICE": true,
}

// privileges lists what a service asks for beyond a normal container: such
// a container can read the host's keys, other stacks' volumes and secrets,
// and change the network rules that keep containers apart.
func privileges(config types.ServiceConfig) []string {
	var asked []string
	if config.Privileged {
		asked = append(asked, "privileged")
	}
	for _, capability := range config.CapAdd {
		name := strings.TrimPrefix(strings.ToUpper(capability), "CAP_")
		if !harmlessCapabilities[name] {
			asked = append(asked, "cap_add "+name)
		}
	}
	for _, option := range config.SecurityOpt {
		if option != "no-new-privileges" && option != "no-new-privileges:true" {
			asked = append(asked, "security_opt "+option)
		}
	}
	if config.CgroupParent != "" {
		asked = append(asked, "cgroup_parent")
	}
	return asked
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
	case port >= 10000 && port <= 10999: // DRBD: 10000 + minor (state.MaxMinor)
		return true
	case port >= 20000 && port <= 29999: // exported services
		return true
	case port >= 51800 && port <= 51831: // WireGuard: 51800 + host ID (state.MaxHosts)
		return true
	default:
		return false
	}
}

// checkUnique: two services of a stack cannot publish the same port or
// take the same HTTP route (the cluster checks the same between stacks).
func checkUnique(services map[string]Service) Problems {
	var problems Problems
	owners := map[string]string{}
	claim := func(service, what, key string) {
		if owner, taken := owners[what+" "+key]; taken {
			problems = append(problems, fmt.Sprintf("service %s: %s %s is %s's already", service, what, key, owner))
			return
		}
		owners[what+" "+key] = service
	}
	for _, name := range slices.Sorted(maps.Keys(services)) {
		for _, port := range services[name].Ports {
			claim(name, "port", port.Key())
		}
		if route := services[name].HTTP; route != nil {
			claim(name, "route", route.Key())
		}
	}
	return problems
}
