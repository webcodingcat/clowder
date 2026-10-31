package stackspec

import (
	"cmp"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// Normalize fills in the schema's own defaults and writes what may be
// written in several ways in one, so two specs written differently but
// meaning the same are equal: lists whose order means nothing are sorted,
// a route's host is in lower case, a capability has no CAP_. Defaults of
// cluster.yaml (reservations, priority) stay out: the scheduler takes them
// when it places. Normalize shares nothing with the spec it gets and leaves
// it as it was; a normalized spec normalizes to itself.
func Normalize(spec Spec) Spec {
	normalized := Spec{Priority: spec.Priority, Services: map[string]Service{}}
	for name, service := range spec.Services {
		normalized.Services[name] = normalizeService(service)
	}
	return normalized
}

func normalizeService(service Service) Service {
	if service.Restart == "" {
		// an orchestrator's service comes back; a one-shot job says no
		service.Restart = "always"
	}
	restartUnhealthy := service.RestartsUnhealthy()
	service.RestartUnhealthy = &restartUnhealthy
	ports := make([]Port, 0, len(service.Ports))
	for _, port := range service.Ports {
		ports = append(ports, port.withDefaults())
	}
	slices.SortFunc(ports, func(a, b Port) int { return strings.Compare(a.key(), b.key()) })
	service.Ports = nilIfEmpty(ports)
	if service.HTTP != nil {
		route := *service.HTTP
		route.Host = strings.ToLower(route.Host)
		if route.Path == "" {
			route.Path = "/"
		}
		service.HTTP = &route
	}
	if service.Healthcheck != nil {
		check := *service.Healthcheck
		check.Command = slices.Clone(check.Command)
		service.Healthcheck = &check
	}
	service.Resources = Resources{
		Reserve: copyAmount(service.Resources.Reserve),
		Limit:   copyAmount(service.Resources.Limit),
	}
	service.Tmpfs = nilIfEmpty(slices.SortedFunc(slices.Values(service.Tmpfs),
		func(a, b Tmpfs) int { return cmp.Compare(a.Path, b.Path) }))
	service.Secrets = sortedSet(service.Secrets)
	service.DependsOn = sortedSet(service.DependsOn)
	service.CapAdd = capabilities(service.CapAdd)
	service.CapDrop = capabilities(service.CapDrop)
	service.SecurityOpt = sortedSet(service.SecurityOpt)
	service.Command = nilIfEmpty(slices.Clone(service.Command))
	service.Entrypoint = nilIfEmpty(slices.Clone(service.Entrypoint))
	service.Volumes = nilIfEmpty(slices.SortedFunc(slices.Values(service.Volumes),
		func(a, b Mount) int { return cmp.Compare(a.Path, b.Path) }))
	service.Env = mapOrNil(service.Env)
	service.SecretEnv = mapOrNil(service.SecretEnv)
	return service
}

func copyAmount(amount *Amount) *Amount {
	if amount == nil {
		return nil
	}
	copied := *amount
	return &copied
}

// capabilities are the names of capabilities as Docker writes them: in
// upper case, without CAP_, sorted, each once.
func capabilities(names []string) []string {
	written := make([]string, 0, len(names))
	for _, name := range names {
		written = append(written, CapabilityName(name))
	}
	return sortedSet(written)
}

// sortedSet is a set's members sorted, each once; nil for none.
func sortedSet(members []string) []string {
	return nilIfEmpty(slices.Compact(slices.Sorted(slices.Values(members))))
}

func nilIfEmpty[T any](list []T) []T {
	if len(list) == 0 {
		return nil
	}
	return list
}

func mapOrNil(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	return maps.Clone(values)
}

// RestartsUnhealthy tells whether the supervisor restarts the service's
// container when it turns unhealthy: unless the spec says no.
func (s Service) RestartsUnhealthy() bool {
	return s.RestartUnhealthy == nil || *s.RestartUnhealthy
}

// withDefaults is the port with the schema's defaults filled in: published
// as the container's port, over tcp.
func (p Port) withDefaults() Port {
	if p.Published == 0 {
		p.Published = p.Port
	}
	if p.Protocol == "" {
		p.Protocol = "tcp"
	}
	return p
}

// key is how the cluster names a published port: "udp/9987".
func (p Port) key() string {
	port := p.withDefaults()
	return port.Protocol + "/" + strconv.Itoa(int(port.Published))
}

// key is how the cluster names an HTTP route: the host in lower case and
// the path ("shop.example.com/api").
func (h HTTP) key() string {
	routePath := h.Path
	if routePath == "" {
		routePath = "/"
	}
	return strings.ToLower(h.Host) + routePath
}

// PublishedPorts are the ports the stack publishes, sorted: no other stack
// may have the same.
func (s Spec) PublishedPorts() []string {
	var ports []string
	for _, service := range s.Services {
		for _, port := range service.Ports {
			ports = append(ports, port.key())
		}
	}
	return sortedSet(ports)
}

// Routes are the stack's HTTP routes, sorted: no other stack may have the
// same.
func (s Spec) Routes() []string {
	var routes []string
	for _, service := range s.Services {
		if service.HTTP != nil {
			routes = append(routes, service.HTTP.key())
		}
	}
	return sortedSet(routes)
}

// VolumeNames are the volumes of the namespace the stack mounts, sorted.
func (s Spec) VolumeNames() []string {
	var volumes []string
	for _, service := range s.Services {
		for _, mount := range service.Volumes {
			volumes = append(volumes, mount.Volume)
		}
	}
	return sortedSet(volumes)
}

// SecretNames are the secrets of the namespace the stack uses, as files
// or variables, sorted.
func (s Spec) SecretNames() []string {
	var secrets []string
	for _, service := range s.Services {
		secrets = append(secrets, service.Secrets...)
		secrets = append(secrets, slices.Collect(maps.Values(service.SecretEnv))...)
	}
	return sortedSet(secrets)
}
