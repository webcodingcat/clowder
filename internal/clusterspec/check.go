package clusterspec

import (
	"fmt"
	"maps"
	"net/mail"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/units"
)

// clowder's own address ranges. They live here, where cluster.yaml is
// checked against them; network and hostcheck take them from here.
var (
	// Overlay holds the node addresses.
	Overlay = netip.MustParsePrefix("10.77.0.0/24")
	// ExportVIPs holds the addresses of exported services.
	ExportVIPs = netip.MustParsePrefix("10.78.0.0/16")
	// ClowderRanges are both: neither the private network nor Docker's
	// address pools may overlap them.
	ClowderRanges = []netip.Prefix{Overlay, ExportVIPs}
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

	problems = append(problems, checkHetzner(config.Hetzner)...)
	network, err := netip.ParsePrefix(config.PrivateNetwork)
	if err != nil || !network.Addr().Is4() || network != network.Masked() {
		report("private_network %q is not an IPv4 network such as 10.0.0.0/16", config.PrivateNetwork)
	}
	for _, used := range ClowderRanges {
		if err == nil && network.Overlaps(used) {
			report("private_network %s overlaps clowder's %s", network, used)
		}
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
	for _, stack := range config.Stacks.Privileged {
		if _, _, ok := names.SplitID(stack); !ok {
			report("stacks.privileged: %q is not <namespace>/<stack> (%s)", stack, names.Rule)
		}
	}
	problems = append(problems, checkResources("defaults.reservations", config.Defaults.Reservations)...)
	problems = append(problems, checkResources("system_reserve", config.SystemReserve)...)
	if config.Alerts != nil {
		for index, channel := range config.Alerts.Channels {
			problems = append(problems, checkChannel(index, channel)...)
		}
		if heartbeat := config.Alerts.Heartbeat; heartbeat != nil {
			if heartbeat.URL == "" {
				report("alerts.heartbeat.url: a value tagged !secret is required")
			}
			if heartbeat.Interval != "" {
				if interval, err := units.ParseDuration(heartbeat.Interval); err != nil || interval <= 0 {
					report("alerts.heartbeat.interval %q is not a positive duration like 5m", heartbeat.Interval)
				}
			}
		}
	}
	problems = append(problems, checkACME(config.ACME)...)
	for _, host := range slices.Sorted(maps.Keys(config.Registries)) {
		if config.Registries[host].Username == "" {
			report("registries[%q].username is required", host)
		}
		if config.Registries[host].Password == "" {
			report("registries[%q].password: a value tagged !secret is required", host)
		}
	}
	for _, place := range SecretPlaces(config) {
		if string(place.Name) != place.Path {
			report("%s names the secret %q; a secret is named by its place", place.Path, place.Name)
		}
	}
	files := FileNames(config)
	for _, name := range files {
		if !validFileName(name) {
			report("%q is not a path relative to %s, written with /", name, FileName)
		}
	}
	if len(files) > maxFiles {
		report("%s names %d files; at most %d are allowed", FileName, len(files), maxFiles)
	}
	return problems
}

// checkHetzner: a token, and an API that is a full http or https URL: the
// token goes there.
func checkHetzner(hetzner Hetzner) Problems {
	var problems Problems
	if hetzner.Token == "" {
		problems = append(problems, "hetzner.token: a value tagged !secret is required")
	}
	if hetzner.API != "" && !fullURL(hetzner.API) {
		problems = append(problems, fmt.Sprintf("hetzner.api %q is not a full URL with http:// or https://", hetzner.API))
	}
	return problems
}

// fullURL tells whether text is a URL with the scheme http or https and a
// host: clowder sends tokens and keys there.
func fullURL(text string) bool {
	parsed, err := url.Parse(text)
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != ""
}

// EndpointWarnings warns about hetzner.api and acme.directory. Unset, clowder
// talks to Hetzner's API and Let's Encrypt over https; set, they point it
// elsewhere, which is meant for test labs, and over http the Hetzner token
// and the ACME requests cross the network unencrypted. The CLI prints them
// as cluster.yaml goes out, the leader logs them.
func EndpointWarnings(config Config) []string {
	var warnings []string
	if config.Hetzner.API != "" {
		warnings = append(warnings, endpointWarning("hetzner.api", config.Hetzner.API,
			"instead of Hetzner's own API", "the Hetzner token"))
	}
	if config.ACME.Directory != "" {
		warnings = append(warnings, endpointWarning("acme.directory", config.ACME.Directory,
			"instead of Let's Encrypt", "the ACME account's requests"))
	}
	return warnings
}

// endpointWarning says that a field points clowder elsewhere, and what
// crosses the network in the clear when the URL is http.
func endpointWarning(field, address, insteadOf, secret string) string {
	warning := fmt.Sprintf("%s is set: clowder uses %s %s, which is meant for test labs", field, address, insteadOf)
	if strings.HasPrefix(address, "http://") {
		warning += "; over http " + secret + " crosses the network unencrypted"
	}
	return warning
}

// checkACME: a full URL for the directory, an address for the email and a
// positive duration for renew_before.
func checkACME(acme ACME) Problems {
	var problems Problems
	if acme.Directory != "" && !fullURL(acme.Directory) {
		problems = append(problems, fmt.Sprintf("acme.directory %q is not a full URL with http:// or https://", acme.Directory))
	}
	if acme.Email != "" {
		if address, err := mail.ParseAddress(acme.Email); err != nil || address.Address != acme.Email {
			problems = append(problems, fmt.Sprintf("acme.email %q is not an email address", acme.Email))
		}
	}
	if acme.RenewBefore != "" {
		if renewBefore, err := units.ParseDuration(acme.RenewBefore); err != nil || renewBefore <= 0 {
			problems = append(problems, fmt.Sprintf("acme.renew_before %q is not a positive duration like 30d", acme.RenewBefore))
		}
	}
	return problems
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
			} else if len(vg) > names.MaxVolumeGroupLength {
				// the volume group is part of each volume's device-mapper name
				problems = append(problems, fmt.Sprintf("storage class %s: volume group %q is longer than %d characters",
					name, vg, names.MaxVolumeGroupLength))
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
	if channel.MinSeverity != "" && !slices.Contains(Severities, channel.MinSeverity) {
		report(fmt.Sprintf("min_severity %q is not info, warning or critical", channel.MinSeverity))
	}
	switch channel.Type {
	case "discord":
		if channel.Webhook == "" {
			report("webhook: a value tagged !secret is required")
		}
	case "webhook":
		if channel.URL == "" {
			report("url: a value tagged !secret is required")
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
