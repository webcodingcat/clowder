package clusterdir

import (
	"fmt"
	"maps"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/webcodingcat/clowder/internal/units"
)

// Problems lists everything wrong in a cluster directory at once.
type Problems []string

func (p Problems) Error() string {
	return "the cluster directory has problems:\n  - " + strings.Join(p, "\n  - ")
}

// Check returns what is wrong in cluster.yaml, or nothing.
func Check(config Config) Problems {
	var problems Problems
	report := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	if config.Name == "" {
		report("name is required")
	}
	if len(config.Seeds) == 0 {
		report("seeds: at least one address is required")
	}
	for _, seed := range config.Seeds {
		if !validSeed(seed) {
			report("seeds: %q is neither an address nor a host name", seed)
		}
	}
	if config.Hoster == "" {
		report("hoster is required")
	}
	if missingSecret(&config.HosterToken) {
		report("hoster_token: {secret: <name>} is required")
	}
	if config.Network != "" && config.Network != "private" && config.Network != "public" {
		report("network %q is neither private nor public", config.Network)
	}
	if address, err := netip.ParseAddr(config.Ingress.FloatingIP); err != nil || !address.Is4() {
		report("ingress.floating_ip %q is not an IPv4 address", config.Ingress.FloatingIP)
	}
	problems = append(problems, checkStorageClasses(config.StorageClasses)...)
	if config.Replication.ResyncRate != "" {
		if _, err := units.ParseSize(config.Replication.ResyncRate); err != nil {
			report("replication.resync_rate: %v", err)
		}
	}
	problems = append(problems, checkPriorities(config.Priorities)...)
	problems = append(problems, checkResources("defaults.reservations", config.Defaults.Reservations)...)
	problems = append(problems, checkResources("system_reserve", config.SystemReserve)...)
	if backup := config.Backup; backup != nil {
		if backup.Target.Type != "restic" {
			report("backup.target.type %q is not supported (restic)", backup.Target.Type)
		}
		if backup.Target.URL == "" {
			report("backup.target.url is required")
		}
		if missingSecret(&backup.Password) {
			report("backup.password: {secret: <name>} is required")
		}
	}
	if config.Alerts != nil {
		for index, channel := range config.Alerts.Channels {
			problems = append(problems, checkChannel(index, channel)...)
		}
		if heartbeat := config.Alerts.Heartbeat; heartbeat != nil {
			if missingSecret(&heartbeat.URL) {
				report("alerts.heartbeat.url: {secret: <name>} is required")
			}
			if heartbeat.Interval != "" {
				if interval, err := time.ParseDuration(heartbeat.Interval); err != nil || interval <= 0 {
					report("alerts.heartbeat.interval %q is not a duration like 5m", heartbeat.Interval)
				}
			}
		}
	}
	if listen := config.Metrics.Listen; listen != "" && listen != "node" && listen != "node+underlay" {
		report("metrics.listen %q is neither node nor node+underlay", listen)
	}
	return problems
}

// validSeed accepts an IP address or a host name, each with an optional port.
func validSeed(seed string) bool {
	host := seed
	if splitHost, port, err := net.SplitHostPort(seed); err == nil {
		if _, err := strconv.ParseUint(port, 10, 16); err != nil {
			return false
		}
		host = splitHost
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return true
	}
	return host != "" && !strings.ContainsAny(host, " /:")
}

// checkStorageClasses: a class has a pool or falls back to other classes,
// which must have pools themselves.
func checkStorageClasses(classes map[string]StorageClass) Problems {
	var problems Problems
	if len(classes) == 0 {
		problems = append(problems, "storage_classes: at least one class is required")
	}
	for _, name := range slices.Sorted(maps.Keys(classes)) {
		class := classes[name]
		switch {
		case class.Pool != "" && len(class.Fallback) > 0:
			problems = append(problems, fmt.Sprintf("storage class %s: either pool or fallback, not both", name))
		case class.Pool != "":
			if vg, lv, ok := strings.Cut(class.Pool, "/"); !ok || vg == "" || lv == "" {
				problems = append(problems, fmt.Sprintf("storage class %s: pool %q is not volume-group/thin-pool", name, class.Pool))
			}
			if class.Reserve != "" && !validPercent(class.Reserve) {
				problems = append(problems, fmt.Sprintf("storage class %s: reserve %q is not a percentage from 0%% to 90%%", name, class.Reserve))
			}
		case len(class.Fallback) > 0:
			for _, target := range class.Fallback {
				if classes[target].Pool == "" {
					problems = append(problems, fmt.Sprintf("storage class %s: falls back to %q, which is not a class with a pool", name, target))
				}
			}
		default:
			problems = append(problems, fmt.Sprintf("storage class %s: needs a pool or a fallback", name))
		}
	}
	return problems
}

// validPercent accepts "0%" to "90%".
func validPercent(text string) bool {
	number, err := strconv.Atoi(strings.TrimSuffix(text, "%"))
	return strings.HasSuffix(text, "%") && err == nil && number >= 0 && number <= 90
}

// checkPriorities: numbers from 0 to 1000, and the default is one of the
// categories. Without categories the default ones apply (withDefaults), so
// the default must be one of those; with own categories it must be named.
func checkPriorities(priorities Priorities) Problems {
	var problems Problems
	for _, name := range slices.Sorted(maps.Keys(priorities.Categories)) {
		if value := priorities.Categories[name]; value < 0 || value > 1000 {
			problems = append(problems, fmt.Sprintf("priority category %s: %d is not from 0 to 1000", name, value))
		}
	}
	if len(priorities.Categories) > 0 && priorities.Default == "" {
		problems = append(problems, "priorities.default is required with own categories")
	}
	effective := priorities.withDefaults()
	if _, ok := effective.Categories[effective.Default]; !ok {
		problems = append(problems, fmt.Sprintf("priorities.default %q is not one of the categories %s",
			effective.Default, strings.Join(slices.Sorted(maps.Keys(effective.Categories)), ", ")))
	}
	return problems
}

// checkResources: a memory size and a positive number of CPUs.
func checkResources(where string, resources Resources) Problems {
	var problems Problems
	if resources.Memory != "" {
		if _, err := units.ParseSize(resources.Memory); err != nil {
			problems = append(problems, fmt.Sprintf("%s.memory: %v", where, err))
		}
	}
	if resources.CPUs != "" {
		if cpus, err := strconv.ParseFloat(resources.CPUs, 64); err != nil || cpus <= 0 {
			problems = append(problems, fmt.Sprintf("%s.cpus %q is not a positive number", where, resources.CPUs))
		}
	}
	return problems
}

// checkChannel: each type needs its own fields.
func checkChannel(index int, channel AlertChannel) Problems {
	var problems Problems
	report := func(problem string) {
		problems = append(problems, fmt.Sprintf("alerts.channels[%d] (%s): %s", index, channel.Type, problem))
	}
	switch channel.MinSeverity {
	case "", "info", "warning", "critical":
	default:
		report(fmt.Sprintf("min_severity %q is not info, warning or critical", channel.MinSeverity))
	}
	switch channel.Type {
	case "discord":
		if missingSecret(channel.Webhook) {
			report("webhook: {secret: <name>} is required")
		}
	case "webhook":
		if missingSecret(channel.URL) {
			report("url: {secret: <name>} is required")
		}
	case "email":
		if channel.SMTP == nil || channel.From == "" || len(channel.To) == 0 {
			report("smtp, from and to are required")
		} else if channel.SMTP.Host == "" || channel.SMTP.Port == 0 {
			report("smtp needs host and port")
		}
	default:
		report("type is not discord, email or webhook")
	}
	return problems
}

// SecretNames lists every secret cluster.yaml refers to, sorted.
func SecretNames(config Config) []string {
	names := map[string]bool{}
	add := func(reference *SecretRef) {
		if reference != nil && reference.Secret != "" {
			names[reference.Secret] = true
		}
	}
	add(&config.HosterToken)
	if config.Backup != nil {
		add(&config.Backup.Password)
	}
	if config.Alerts != nil {
		for _, channel := range config.Alerts.Channels {
			add(channel.Webhook)
			add(channel.URL)
			if channel.SMTP != nil {
				add(channel.SMTP.Password)
			}
		}
		if config.Alerts.Heartbeat != nil {
			add(&config.Alerts.Heartbeat.URL)
		}
	}
	return slices.Sorted(maps.Keys(names))
}

// missingSecret tells whether a reference to a secret is absent or names none.
func missingSecret(reference *SecretRef) bool {
	return reference == nil || reference.Secret == ""
}
