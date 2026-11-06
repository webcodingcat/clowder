package stackspec

import (
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/webcodingcat/clowder/internal/clusterspec"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/units"
)

// routePath is an absolute URL path of only the characters that RFC 3986
// allows. It goes into a Traefik rule between backticks, so it must not
// hold a backtick.
var routePath = regexp.MustCompile(`^/[A-Za-z0-9._~!$&'()*+,;=:@%/-]*$`)

// signal is a signal's name in the form that Docker accepts.
var signal = regexp.MustCompile(`^SIG[A-Z0-9]+$`)

// Check lists what is wrong with a spec by itself. What only the cluster
// knows (categories, privileges, the namespace's volumes and secrets) is
// checked against the cluster.
func Check(spec Spec) Problems {
	var problems Problems
	report := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}
	if len(spec.Services) == 0 {
		report("the stack has no services")
	}
	if spec.Priority != "" {
		if _, err := ParsePriority(spec.Priority); err != nil {
			report("priority: %v", err)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(spec.Services)) {
		for _, problem := range checkService(name, spec.Services[name], spec.Services) {
			report("service %s: %s", name, problem)
		}
	}
	problems = append(problems, checkUnique(spec.Services)...)
	problems = append(problems, checkCycles(spec.Services)...)
	return problems
}

func checkService(name string, service Service, services map[string]Service) []string {
	var problems []string
	report := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}
	if !names.Valid(name) {
		// it becomes part of container names, Traefik's names and DNS
		report("the name is not a service name (%s)", names.Rule)
	}
	if strings.TrimSpace(service.Image) == "" {
		report("image is required")
	}
	if service.Priority != "" {
		if _, err := ParsePriority(service.Priority); err != nil {
			report("priority: %v", err)
		}
	}
	problems = append(problems, checkEnv(service)...)
	problems = append(problems, checkMounts(service)...)
	for _, port := range service.Ports {
		if problem := checkPort(port); problem != "" {
			problems = append(problems, problem)
		}
	}
	if service.HTTP != nil {
		problems = append(problems, checkHTTP(*service.HTTP)...)
	}
	for _, dependency := range service.DependsOn {
		if _, ok := services[dependency]; !ok || dependency == name {
			report("dependsOn: %q is no other service of the stack", dependency)
		}
	}
	if service.Healthcheck != nil {
		problems = append(problems, checkHealthcheck(*service.Healthcheck)...)
	}
	if service.Restart != "" && !ValidRestart(service.Restart) {
		report("restart %q is not one of always, no, on-failure, on-failure:<n>", service.Restart)
	}
	problems = append(problems, checkResources(service.Resources)...)
	if service.StopGracePeriod != "" {
		if _, err := time.ParseDuration(service.StopGracePeriod); err != nil {
			report("stopGracePeriod %q is not a duration like 10s", service.StopGracePeriod)
		}
	}
	if service.StopSignal != "" && !signal.MatchString(service.StopSignal) {
		report("stopSignal %q is not a signal's name like SIGTERM", service.StopSignal)
	}
	if service.ShmSize != "" {
		if _, err := units.ParseSize(service.ShmSize); err != nil {
			report("shmSize: %v", err)
		}
	}
	for _, capability := range slices.Concat(service.CapAdd, service.CapDrop) {
		if capability == "" || strings.ContainsAny(capability, " \t") {
			report("capability %q is not a capability's name", capability)
		}
	}
	return problems
}

func checkEnv(service Service) []string {
	var problems []string
	report := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}
	for _, variable := range slices.Sorted(maps.Keys(service.Env)) {
		if !names.ValidVariable(variable) {
			report("env: %q is not a variable name", variable)
		}
	}
	for _, variable := range slices.Sorted(maps.Keys(service.SecretEnv)) {
		if !names.ValidVariable(variable) {
			report("secretEnv: %q is not a variable name", variable)
		} else if problem := service.SecretEnv[variable].Problem(); problem != "" {
			report("secretEnv.%s: %s", variable, problem)
		}
		if _, ok := service.Env[variable]; ok {
			report("secretEnv: %s is set in env as well", variable)
		}
	}
	for _, secret := range service.Secrets {
		if !names.Valid(secret) {
			report("secrets: %q is not a secret name (%s)", secret, names.Rule)
		}
	}
	if len(sortedSet(service.Secrets)) != len(service.Secrets) {
		report("secrets: a secret is named twice")
	}
	return problems
}

func checkMounts(service Service) []string {
	var problems []string
	report := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}
	paths := map[string]bool{}
	claim := func(what, mountPath string) {
		if !path.IsAbs(mountPath) || path.Clean(mountPath) != mountPath {
			report("%s: %q is not a clean absolute path", what, mountPath)
		}
		if paths[mountPath] {
			report("%s: %s is mounted twice", what, mountPath)
		}
		paths[mountPath] = true
	}
	for _, mount := range service.Volumes {
		if !names.Valid(mount.Volume) {
			report("volumes: %q is not a volume name (%s)", mount.Volume, names.Rule)
		}
		claim("volumes", mount.Path)
	}
	for _, tmpfs := range service.Tmpfs {
		claim("tmpfs", tmpfs.Path)
		if tmpfs.Size != "" {
			if _, err := units.ParseSize(tmpfs.Size); err != nil {
				report("tmpfs %s: %v", tmpfs.Path, err)
			}
		}
	}
	if service.Localtime {
		claim("localtime", "/etc/localtime")
	}
	return problems
}

func checkPort(port Port) string {
	published := port.withDefaults().Published
	switch {
	case port.Port == 0:
		return "ports: a port is required"
	case port.Protocol != "" && port.Protocol != "tcp" && port.Protocol != "udp":
		return fmt.Sprintf("ports: protocol %q is not tcp or udp", port.Protocol)
	case ReservedPort(published):
		return fmt.Sprintf("ports: %d is reserved for clowder or the host", published)
	default:
		return ""
	}
}

// checkHTTP is strict because the host and the path go into a Traefik
// rule.
func checkHTTP(route HTTP) []string {
	var problems []string
	if !names.ValidDomain(route.Host) {
		problems = append(problems, fmt.Sprintf("http: host %q is not a domain name", route.Host))
	}
	if route.Port == 0 {
		problems = append(problems, "http: a port is required")
	}
	if route.Path != "" && !ValidRoutePath(route.Path) {
		problems = append(problems, fmt.Sprintf(
			"http: path %q must start with / and hold only the characters of a URL path", route.Path))
	}
	return problems
}

func checkHealthcheck(check Healthcheck) []string {
	var problems []string
	if len(check.Command) == 0 {
		problems = append(problems, "healthcheck: a command is required")
	}
	durations := []struct{ what, value string }{
		{"interval", check.Interval}, {"timeout", check.Timeout}, {"startPeriod", check.StartPeriod},
	}
	for _, duration := range durations {
		if duration.value == "" {
			continue
		}
		if parsed, err := time.ParseDuration(duration.value); err != nil || parsed <= 0 {
			problems = append(problems, fmt.Sprintf(
				"healthcheck: %s %q is not a duration like 10s", duration.what, duration.value))
		}
	}
	if check.Retries < 0 {
		problems = append(problems, "healthcheck: retries is negative")
	}
	return problems
}

func checkResources(resources Resources) []string {
	var problems []string
	amounts := []struct {
		what   string
		amount *Amount
	}{{"reserve", resources.Reserve}, {"limit", resources.Limit}}
	for _, entry := range amounts {
		what, amount := entry.what, entry.amount
		if amount == nil {
			continue
		}
		if amount.Memory != "" {
			if _, err := units.ParseSize(amount.Memory); err != nil {
				problems = append(problems, fmt.Sprintf("resources.%s.memory: %v", what, err))
			}
		}
		if amount.CPUs < 0 {
			problems = append(problems, fmt.Sprintf("resources.%s.cpus is negative", what))
		}
	}
	return problems
}

// checkUnique checks within the stack; the state checks the same between
// stacks.
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
		service := services[name]
		for _, port := range service.Ports {
			claim(name, "port", port.key())
		}
		if service.HTTP != nil {
			claim(name, "route", service.HTTP.key())
		}
	}
	return problems
}

// checkCycles refuses services that depend on each other in a circle,
// because Compose could start none of them. It names one service of each
// circle, not the services that only depend on one.
func checkCycles(services map[string]Service) Problems {
	const (
		visiting = 1 // on the path from where the search started
		done     = 2 // in no circle, or in a circle already reported
	)
	state := map[string]int{}
	var problems Problems
	// visit returns a service of the circle that it found, or "".
	var visit func(name string) string
	visit = func(name string) string {
		switch state[name] {
		case visiting:
			return name // back on the path: name is in the circle
		case done:
			return ""
		}
		state[name] = visiting
		for _, dependency := range services[name].DependsOn {
			if _, ok := services[dependency]; !ok {
				continue // checkService reports it
			}
			if member := visit(dependency); member != "" {
				return member
			}
		}
		state[name] = done
		return ""
	}
	for _, name := range slices.Sorted(maps.Keys(services)) {
		if state[name] != 0 {
			continue
		}
		if member := visit(name); member != "" {
			problems = append(problems, fmt.Sprintf("service %s: dependsOn goes in a circle", member))
			// one report per circle: the services still on the path are
			// either in the circle or depend on it
			for other, seen := range state {
				if seen == visiting {
					state[other] = done
				}
			}
		}
	}
	return problems
}

// ParsePriority reads a priority: a whole number from 0 to 1000, or a
// category's name, which it does not look up. "" gives the zero Priority,
// the cluster's default.
func ParsePriority(text string) (clusterspec.Priority, error) {
	if text == "" {
		return clusterspec.Priority{}, nil
	}
	if number, err := strconv.Atoi(text); err == nil {
		if number < 0 || number > 1000 {
			return clusterspec.Priority{}, fmt.Errorf("%d is not from 0 to 1000", number)
		}
		return clusterspec.Priority{Value: &number}, nil
	}
	if !names.Valid(text) {
		return clusterspec.Priority{}, fmt.Errorf("%q is neither a number from 0 to 1000 nor a category's name", text)
	}
	return clusterspec.Priority{Category: text}, nil
}

// ValidRoutePath tells whether a path may follow an HTTP route's host.
func ValidRoutePath(routePathText string) bool {
	return routePath.MatchString(routePathText)
}

// ValidRestart tells whether a restart rule is one the supervisor applies.
func ValidRestart(restart string) bool {
	switch {
	case restart == "always", restart == "no", restart == "on-failure":
		return true
	case strings.HasPrefix(restart, "on-failure:"):
		count, err := strconv.Atoi(strings.TrimPrefix(restart, "on-failure:"))
		return err == nil && count > 0
	default:
		return false
	}
}

// ReservedPort tells whether the host or clowder itself uses a port.
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
