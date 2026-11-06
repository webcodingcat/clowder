package clusterspec

import (
	"crypto/x509"
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

// clowder's own address ranges. They are defined here because Check tests
// the configuration against them. network and hostcheck use them too.
var (
	// Overlay holds the node addresses.
	Overlay = netip.MustParsePrefix("10.77.0.0/24")
	// ExportVIPs holds the addresses of exported services.
	ExportVIPs = netip.MustParsePrefix("10.78.0.0/16")
	// ClowderRanges are both ranges. Neither the private network nor
	// Docker's address pools may overlap them.
	ClowderRanges = []netip.Prefix{Overlay, ExportVIPs}
)

// MaxCASize limits a CA in the configuration (acme.ca, smtp.ca). The
// configuration is part of the state, and every agent keeps the state in
// memory.
const MaxCASize = 64 << 10

// Problems lists everything wrong in a configuration at once.
type Problems []string

func (p Problems) Error() string {
	return "the configuration has problems:\n  - " + strings.Join(p, "\n  - ")
}

// Check returns what is wrong in the configuration by itself, or nothing.
// It does not check that the named Secrets exist; the state does that.
func Check(config Config) Problems {
	var problems Problems
	report := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	if config.Hetzner.API != "" && !fullURL(config.Hetzner.API) {
		report("hetzner.api %q is not a full URL with http:// or https://", config.Hetzner.API)
	}
	network, err := netip.ParsePrefix(config.PrivateNetwork)
	if err != nil || !network.Addr().Is4() || network != network.Masked() {
		report("privateNetwork %q is not an IPv4 network such as 10.0.0.0/16", config.PrivateNetwork)
	}
	for _, used := range ClowderRanges {
		if err == nil && network.Overlaps(used) {
			report("privateNetwork %s overlaps clowder's %s", network, used)
		}
	}
	if address, err := netip.ParseAddr(config.Ingress.FloatingIP); err != nil || !address.Is4() {
		report("ingress.floatingIP %q is not an IPv4 address", config.Ingress.FloatingIP)
	}
	problems = append(problems, checkStorageClasses(config.StorageClasses)...)
	if config.Replication.ResyncRate != "" {
		if _, err := units.ParseSize(config.Replication.ResyncRate); err != nil {
			report("replication.resyncRate: %v", err)
		}
	}
	problems = append(problems, checkPriorities(config.Priorities)...)
	for _, stack := range config.Stacks.Privileged {
		if _, _, ok := names.SplitID(stack); !ok {
			report("stacks.privileged: %q is not <namespace>/<stack> (%s)", stack, names.Rule)
		}
	}
	problems = append(problems, checkResources("defaults.reservations", config.Defaults.Reservations)...)
	problems = append(problems, checkResources("systemReserve", config.SystemReserve)...)
	if config.Alerts != nil {
		for index, channel := range config.Alerts.Channels {
			problems = append(problems, checkChannel(index, channel)...)
		}
		if heartbeat := config.Alerts.Heartbeat; heartbeat != nil && heartbeat.Interval != "" {
			if interval, err := units.ParseDuration(heartbeat.Interval); err != nil || interval <= 0 {
				report("alerts.heartbeat.interval %q is not a positive duration like 5m", heartbeat.Interval)
			}
		}
	}
	problems = append(problems, checkACME(config.ACME)...)
	for _, place := range SecretPlaces(config) {
		if problem := place.Ref.Problem(); problem != "" {
			report("%s: %s", place.Path, problem)
		}
	}
	return problems
}

// fullURL tells whether text is a URL with the scheme http or https and a
// host. It must be one, because clowder sends tokens and keys there.
func fullURL(text string) bool {
	parsed, err := url.Parse(text)
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != ""
}

// checkCA checks that a CA, if there is one, holds a PEM certificate and
// fits within MaxCASize.
func checkCA(path, ca string) Problems {
	switch {
	case ca == "":
		return nil
	case len(ca) > MaxCASize:
		return Problems{fmt.Sprintf("%s is larger than %d KiB", path, MaxCASize>>10)}
	case !x509.NewCertPool().AppendCertsFromPEM([]byte(ca)):
		return Problems{path + " holds no PEM certificate"}
	}
	return nil
}

// ServiceURLWarnings warns about hetzner.api and acme.directory, the URLs
// of the services that clowder talks to. When they are unset, clowder
// talks to Hetzner's API and Let's Encrypt over https. When they are set,
// they point clowder elsewhere, which is meant for test labs. Over http,
// the Hetzner token and the ACME requests cross the network unencrypted.
// An apply returns the warnings, and the leader logs them.
func ServiceURLWarnings(config Config) []string {
	var warnings []string
	if config.Hetzner.API != "" {
		warnings = append(warnings, serviceURLWarning("hetzner.api", config.Hetzner.API,
			"instead of Hetzner's own API", "the Hetzner token"))
	}
	if config.ACME.Directory != "" {
		warnings = append(warnings, serviceURLWarning("acme.directory", config.ACME.Directory,
			"instead of Let's Encrypt", "the ACME account's requests"))
	}
	return warnings
}

// serviceURLWarning says that a field points clowder elsewhere. For an
// http URL, it also says what crosses the network unencrypted.
func serviceURLWarning(field, address, insteadOf, secret string) string {
	warning := fmt.Sprintf("%s is set: clowder uses %s %s, which is meant for test labs", field, address, insteadOf)
	if strings.HasPrefix(address, "http://") {
		warning += "; over http " + secret + " crosses the network unencrypted"
	}
	return warning
}

// checkACME checks that the directory is a full URL, the CA is valid, the
// email is an address, and renewBefore is a positive duration.
func checkACME(acme ACME) Problems {
	var problems Problems
	if acme.Directory != "" && !fullURL(acme.Directory) {
		problems = append(problems, fmt.Sprintf("acme.directory %q is not a full URL with http:// or https://", acme.Directory))
	}
	problems = append(problems, checkCA("acme.ca", acme.CA)...)
	if acme.Email != "" {
		if address, err := mail.ParseAddress(acme.Email); err != nil || address.Address != acme.Email {
			problems = append(problems, fmt.Sprintf("acme.email %q is not an email address", acme.Email))
		}
	}
	if acme.RenewBefore != "" {
		if renewBefore, err := units.ParseDuration(acme.RenewBefore); err != nil || renewBefore <= 0 {
			problems = append(problems, fmt.Sprintf("acme.renewBefore %q is not a positive duration like 30d", acme.RenewBefore))
		}
	}
	return problems
}

// checkStorageClasses checks that each class either has a pool or falls
// back to other classes. Those other classes must have pools themselves.
func checkStorageClasses(classes map[string]StorageClass) Problems {
	var problems Problems
	if len(classes) == 0 {
		problems = append(problems, "storageClasses: at least one class is required")
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

// checkPriorities checks that the numbers are from 0 to 1000 and that the
// default is one of the categories. A configuration without categories
// gets the default categories (withDefaults), so its default must be one
// of those. A configuration with its own categories must name a default.
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

// checkResources checks that memory is a size and CPUs is a positive
// number.
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

// checkChannel checks that a channel has the fields of its type, and no
// other type's webhook or URL.
func checkChannel(index int, channel AlertChannel) Problems {
	var problems Problems
	report := func(problem string) {
		problems = append(problems, fmt.Sprintf("alerts.channels[%d] (%s): %s", index, channel.Type, problem))
	}
	if channel.MinSeverity != "" && !slices.Contains(Severities, channel.MinSeverity) {
		report(fmt.Sprintf("minSeverity %q is not info, warning or critical", channel.MinSeverity))
	}
	switch channel.Type {
	case "discord":
		if channel.WebhookSecretRef == nil {
			report("webhookSecretRef is required")
		}
	case "webhook":
		if channel.URLSecretRef == nil {
			report("urlSecretRef is required")
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
	if channel.Type != "discord" && channel.WebhookSecretRef != nil {
		report("webhookSecretRef is a discord channel's")
	}
	if channel.Type != "webhook" && channel.URLSecretRef != nil {
		report("urlSecretRef is a webhook channel's")
	}
	if channel.SMTP != nil {
		for _, problem := range checkCA("smtp.ca", channel.SMTP.CA) {
			report(problem)
		}
	}
	return problems
}
