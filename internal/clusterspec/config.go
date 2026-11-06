// Package clusterspec holds the spec of the ClusterConfiguration resource:
// its types, their defaults and Check. It does no I/O, so the state can
// read and check the configuration itself.
package clusterspec

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/units"
)

// Config is the spec of the cluster's one ClusterConfiguration. It holds
// no secret; it names Secrets in names.System instead. Users write the
// json names, and the state stores them, so a Go field may be renamed,
// but its tags never change.
type Config struct {
	Hetzner Hetzner `json:"hetzner"`
	// PrivateNetwork is the Hetzner private network that the hosts share,
	// as a CIDR. Each host's underlay address is in it. Containers may not
	// reach it.
	PrivateNetwork string                  `json:"privateNetwork"`
	Ingress        Ingress                 `json:"ingress"`
	StorageClasses map[string]StorageClass `json:"storageClasses"`
	Replication    Replication             `json:"replication,omitzero"`
	Priorities     Priorities              `json:"priorities,omitzero"`
	Stacks         Stacks                  `json:"stacks,omitzero"`
	Defaults       Defaults                `json:"defaults,omitzero"`
	SystemReserve  Resources               `json:"systemReserve,omitzero"`
	Alerts         *Alerts                 `json:"alerts,omitempty"`
	ACME           ACME                    `json:"acme,omitzero"`
}

// Priorities are the categories that a stack or service may name instead
// of a number. A configuration without this block gets DefaultPriorities.
type Priorities struct {
	// Default is the category of a stack that names no priority.
	Default string `json:"default,omitempty"`
	// Categories map names to numbers from 0 to 1000.
	Categories map[string]int `json:"categories,omitempty"`
}

// Priority is a priority as a stack or service writes it: a number from 0
// to 1000 or the name of a category. The zero value means "the cluster's
// default". Resolve turns it into a number with the cluster's categories.
type Priority struct {
	Value    *int   `json:"value,omitempty"`
	Category string `json:"category,omitempty"`
}

// Resolve turns a priority into a number. An empty priority gets the
// default category.
func (p Priorities) Resolve(priority Priority) (int, error) {
	category := priority.Category
	switch {
	case priority.Value != nil:
		return *priority.Value, nil
	case category == "":
		category = p.Default
	}
	value, ok := p.Categories[category]
	if !ok {
		return 0, fmt.Errorf("the cluster has no priority category %q", category)
	}
	return value, nil
}

// DefaultPriorities returns the categories for a configuration that names
// none.
func DefaultPriorities() Priorities {
	return Priorities{
		Default:    "normal",
		Categories: map[string]int{"critical": 900, "high": 700, "normal": 500, "low": 200},
	}
}

// WithDefaults returns the config with defaults for the fields that users
// may leave out. The state calls it when it applies a configuration.
func (c Config) WithDefaults() Config {
	c.Priorities = c.Priorities.withDefaults()
	c.Defaults.Reservations = c.Defaults.Reservations.withDefaults(DefaultReservations)
	c.SystemReserve = c.SystemReserve.withDefaults(DefaultSystemReserve)
	if c.Replication.ResyncRate == "" {
		c.Replication.ResyncRate = DefaultResyncRate
	}
	return c
}

// The default resources. DefaultReservations is what a service reserves
// without resources.reserve. DefaultSystemReserve is what a host keeps for
// itself (the system, Docker, clowderd, Traefik).
var (
	DefaultReservations  = Resources{Memory: "128M", CPUs: "0.1"}
	DefaultSystemReserve = Resources{Memory: "512M", CPUs: "0.25"}
)

// DefaultResyncRate is how fast a copy resyncs when the configuration has
// no replication.resyncRate, in bytes per second for each resync.
const DefaultResyncRate = "50M"

func (r Resources) withDefaults(defaults Resources) Resources {
	if r.Memory == "" {
		r.Memory = defaults.Memory
	}
	if r.CPUs == "" {
		r.CPUs = defaults.CPUs
	}
	return r
}

// withDefaults keeps a default category that the configuration names,
// even without categories.
func (p Priorities) withDefaults() Priorities {
	if len(p.Categories) > 0 {
		return p
	}
	defaults := DefaultPriorities()
	if p.Default != "" {
		defaults.Default = p.Default
	}
	return defaults
}

// Hetzner is the Hetzner Cloud project the hosts run in.
type Hetzner struct {
	// TokenSecretRef names the project's API token.
	TokenSecretRef names.SecretKeyRef `json:"tokenSecretRef"`
	// API is the full URL of another Hetzner Cloud API than Hetzner's own,
	// such as a test lab.
	API string `json:"api,omitempty"`
}

// Ingress is the Floating IP and the hosts that should hold it.
type Ingress struct {
	FloatingIP     string   `json:"floatingIP"`
	PreferredHosts []string `json:"preferredHosts,omitempty"`
}

// StorageClass is either a thin pool on each host ("vg/thinpool") or a
// fallback list of other classes.
type StorageClass struct {
	Pool string `json:"pool,omitempty"`
	// Reserve is the part of the pool that stays free for snapshots and
	// metadata, such as "20%".
	Reserve  string   `json:"reserve,omitempty"`
	Fallback []string `json:"fallback,omitempty"`
}

// DefaultReservePercent is a pool's reserve when its class names none.
const DefaultReservePercent = 20

// ReservePercent returns the class's reserve in percent of its pool. It
// relies on Check to make sure that Reserve parses.
func (c StorageClass) ReservePercent() int {
	if number, err := strconv.Atoi(strings.TrimSuffix(c.Reserve, "%")); err == nil {
		return number
	}
	return DefaultReservePercent
}

// Replication limits DRBD's resync, so that it never crowds out the
// cluster's own traffic.
type Replication struct {
	// ResyncRate is the most that a resync of a volume copy sends per
	// second, as a size such as "50M".
	ResyncRate string `json:"resyncRate,omitempty"`
}

// Stacks holds what the cluster allows single stacks beyond the rules for
// all stacks.
type Stacks struct {
	// Privileged names the stacks that may use privileged, capabilities
	// beyond the harmless ones, and securityOpt. Such a stack can reach
	// everything on its host, including the data and secrets of other
	// stacks.
	Privileged []string `json:"privileged,omitempty"`
}

// Defaults apply to services without their own values.
type Defaults struct {
	Reservations Resources `json:"reservations,omitzero"`
}

// Resources are memory (such as "128M") and CPUs (such as "0.1").
type Resources struct {
	Memory string `json:"memory,omitempty"`
	CPUs   string `json:"cpus,omitempty"`
}

// Alerts are the channels and the optional heartbeat.
type Alerts struct {
	Channels  []AlertChannel `json:"channels,omitempty"`
	Heartbeat *Heartbeat     `json:"heartbeat,omitempty"`
}

// AlertChannel is one notifier: discord, email or webhook. Its URL is kept
// in a Secret, because anyone with a Discord webhook's URL can post to it.
type AlertChannel struct {
	Type        string `json:"type"`
	MinSeverity string `json:"minSeverity,omitempty"`
	// WebhookSecretRef names a discord channel's webhook URL.
	WebhookSecretRef *names.SecretKeyRef `json:"webhookSecretRef,omitempty"`
	// URLSecretRef names a webhook channel's URL.
	URLSecretRef *names.SecretKeyRef `json:"urlSecretRef,omitempty"`
	SMTP         *SMTP               `json:"smtp,omitempty"`
	From         string              `json:"from,omitempty"`
	To           []string            `json:"to,omitempty"`
}

// The severities of an alert. A channel's minSeverity names one of them.
const (
	SeverityInfo     = "info"
	SeverityWarning  = "warning"
	SeverityCritical = "critical"
)

// Severities are the severities, lowest first.
var Severities = []string{SeverityInfo, SeverityWarning, SeverityCritical}

// AtLeast tells whether severity is min or above. An empty min means info.
func AtLeast(severity, min string) bool {
	if min == "" {
		min = SeverityInfo
	}
	return slices.Index(Severities, severity) >= slices.Index(Severities, min)
}

// SMTP is the mail server of an email channel. Port 465 uses TLS from the
// start; any other port must offer STARTTLS.
type SMTP struct {
	Host              string              `json:"host"`
	Port              int                 `json:"port"`
	User              string              `json:"user,omitempty"`
	PasswordSecretRef *names.SecretKeyRef `json:"passwordSecretRef,omitempty"`
	// CA is a PEM certificate that the leader trusts for the server, in
	// addition to the system's CAs. It is meant for a test CA, like
	// Kubernetes' caBundle.
	CA string `json:"ca,omitempty"`
}

// Heartbeat is a URL that the leader calls at an interval. An external
// service raises an alarm when the calls stop.
type Heartbeat struct {
	// URLSecretRef names the URL. The URL is kept in a Secret, because
	// anyone who has it can fake the heartbeat.
	URLSecretRef names.SecretKeyRef `json:"urlSecretRef"`
	// Interval is a duration such as "1m", which is the default.
	Interval string `json:"interval,omitempty"`
}

// DefaultHeartbeatInterval is how often the heartbeat is called when the
// configuration names no interval.
const DefaultHeartbeatInterval = time.Minute

// Every returns how often the heartbeat is called. It relies on Check to
// make sure that Interval parses.
func (h Heartbeat) Every() time.Duration {
	if interval, err := units.ParseDuration(h.Interval); err == nil && interval > 0 {
		return interval
	}
	return DefaultHeartbeatInterval
}

// ACME says where certificates come from. If it is empty, they come from
// Let's Encrypt.
type ACME struct {
	// Directory is the full URL of the ACME directory.
	Directory string `json:"directory,omitempty"`
	// CA is a PEM certificate that the leader trusts for the directory, in
	// addition to the system's CAs. It is meant for a test CA, like
	// Kubernetes' caBundle.
	CA string `json:"ca,omitempty"`
	// Email is the account's contact for the CA's notices.
	Email string `json:"email,omitempty"`
	// RenewBefore is how long before it expires a certificate is renewed,
	// such as "30d". By default, a certificate is renewed when a third of
	// its lifetime is left.
	RenewBefore string `json:"renewBefore,omitempty"`
}

// DefaultACMEDirectory is Let's Encrypt's directory.
const DefaultACMEDirectory = "https://acme-v02.api.letsencrypt.org/directory"

// DirectoryURL returns the URL of the ACME directory to use.
func (a ACME) DirectoryURL() string {
	if a.Directory == "" {
		return DefaultACMEDirectory
	}
	return a.Directory
}

// SecretPlace is a field of the configuration that names a value of a
// Secret in names.System. Path is the field's path for messages, and Ref
// is the reference in it.
type SecretPlace struct {
	Path string
	Ref  names.SecretKeyRef
}

// SecretPlaces lists the fields of config that name a value of a Secret
// and are set, in the order of the configuration. It is the only list of
// these fields: Check and the state rely on it.
func SecretPlaces(config Config) []SecretPlace {
	places := []SecretPlace{{Path: "hetzner.tokenSecretRef", Ref: config.Hetzner.TokenSecretRef}}
	if config.Alerts == nil {
		return places
	}
	add := func(path string, ref *names.SecretKeyRef) {
		if ref != nil {
			places = append(places, SecretPlace{Path: path, Ref: *ref})
		}
	}
	for index, channel := range config.Alerts.Channels {
		path := fmt.Sprintf("alerts.channels[%d]", index)
		add(path+".webhookSecretRef", channel.WebhookSecretRef)
		add(path+".urlSecretRef", channel.URLSecretRef)
		if channel.SMTP != nil {
			add(path+".smtp.passwordSecretRef", channel.SMTP.PasswordSecretRef)
		}
	}
	if heartbeat := config.Alerts.Heartbeat; heartbeat != nil {
		add("alerts.heartbeat.urlSecretRef", &heartbeat.URLSecretRef)
	}
	return places
}
