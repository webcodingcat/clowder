package stackspec

import (
	"cmp"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/webcodingcat/clowder/internal/names"
)

// Normalize fills in the schema's own defaults. It also writes everything
// that can be written in several ways in one way, so that two specs that
// mean the same are equal. For example, it sorts the lists whose order
// means nothing, writes a route's host in lower case, and drops CAP_ from
// capabilities. It leaves out the configuration's defaults (reservations,
// priority), because the scheduler applies them when it places. The
// result shares nothing with spec, and spec stays unchanged. A normalized
// spec normalizes to itself.
func Normalize(spec Spec) Spec {
	normalized := Spec{Priority: spec.Priority, Services: map[string]Service{}}
	for name, service := range spec.Services {
		normalized.Services[name] = normalizeService(service)
	}
	return normalized
}

func normalizeService(service Service) Service {
	if service.Restart == "" {
		// an orchestrator restarts a service; a one-shot job must say no
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

// capabilities returns the names of capabilities as Docker writes them:
// in upper case, without CAP_, sorted, and each once.
func capabilities(names []string) []string {
	written := make([]string, 0, len(names))
	for _, name := range names {
		written = append(written, CapabilityName(name))
	}
	return sortedSet(written)
}

// sortedSet returns a set's members sorted and each once, or nil if there
// are none.
func sortedSet(members []string) []string {
	return nilIfEmpty(slices.Compact(slices.Sorted(slices.Values(members))))
}

func nilIfEmpty[T any](list []T) []T {
	if len(list) == 0 {
		return nil
	}
	return list
}

func mapOrNil[Value any](values map[string]Value) map[string]Value {
	if len(values) == 0 {
		return nil
	}
	return maps.Clone(values)
}

// RestartsUnhealthy tells whether the supervisor restarts the service's
// container when it turns unhealthy. It does, unless the spec says no.
func (s Service) RestartsUnhealthy() bool {
	return s.RestartUnhealthy == nil || *s.RestartUnhealthy
}

// withDefaults returns the port with the schema's defaults: the published
// port is the container's port, and the protocol is tcp.
func (p Port) withDefaults() Port {
	if p.Published == 0 {
		p.Published = p.Port
	}
	if p.Protocol == "" {
		p.Protocol = "tcp"
	}
	return p
}

// key returns the name that the cluster uses for a published port:
// "udp/9987".
func (p Port) key() string {
	port := p.withDefaults()
	return port.Protocol + "/" + strconv.Itoa(int(port.Published))
}

// key returns the name that the cluster uses for an HTTP route: the host
// in lower case and the path ("shop.example.com/api").
func (h HTTP) key() string {
	routePath := h.Path
	if routePath == "" {
		routePath = "/"
	}
	return strings.ToLower(h.Host) + routePath
}

// PublishedPorts returns the ports that the stack publishes, sorted. No
// other stack may publish the same ports.
func (s Spec) PublishedPorts() []string {
	var ports []string
	for _, service := range s.Services {
		for _, port := range service.Ports {
			ports = append(ports, port.key())
		}
	}
	return sortedSet(ports)
}

// Routes returns the stack's HTTP routes, sorted. No other stack may have
// the same routes.
func (s Spec) Routes() []string {
	var routes []string
	for _, service := range s.Services {
		if service.HTTP != nil {
			routes = append(routes, service.HTTP.key())
		}
	}
	return sortedSet(routes)
}

// VolumeNames returns the volumes of the namespace that the stack mounts,
// sorted.
func (s Spec) VolumeNames() []string {
	var volumes []string
	for _, service := range s.Services {
		for _, mount := range service.Volumes {
			volumes = append(volumes, mount.Volume)
		}
	}
	return sortedSet(volumes)
}

// WithDependents returns the named services and every service that depends
// on one of them, directly or through others, sorted. A dependency only
// applies when a container starts, so whoever restarts a service must also
// restart the services that depend on it.
func (s Spec) WithDependents(services []string) []string {
	with := map[string]bool{}
	for _, service := range services {
		with[service] = true
	}
	for added := true; added; {
		added = false
		for name, service := range s.Services {
			if !with[name] && slices.ContainsFunc(service.DependsOn, func(dependency string) bool { return with[dependency] }) {
				with[name], added = true, true
			}
		}
	}
	return slices.Sorted(maps.Keys(with))
}

// SecretNames returns the namespace's secrets that the stack uses, as files
// or as variables, sorted.
func (s Spec) SecretNames() []string {
	var secrets []string
	for _, service := range s.Services {
		secrets = append(secrets, service.Secrets...)
		for _, ref := range service.SecretEnv {
			secrets = append(secrets, ref.Name)
		}
	}
	return sortedSet(secrets)
}

// SecretKeyRefs returns the single values of secrets that the stack uses
// as variables, sorted and each once. A secret must keep these keys.
func (s Spec) SecretKeyRefs() []names.SecretKeyRef {
	var refs []names.SecretKeyRef
	for _, service := range s.Services {
		refs = append(refs, slices.Collect(maps.Values(service.SecretEnv))...)
	}
	slices.SortFunc(refs, func(a, b names.SecretKeyRef) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.Key, b.Key))
	})
	return slices.Compact(refs)
}
