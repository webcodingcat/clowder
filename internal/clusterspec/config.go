// Package clusterspec is cluster.yaml as clowder reads it: its types, their
// defaults and Check. It does no I/O, so the state reads and checks the
// config itself; reading the cluster directory is clusterdir's.
package clusterspec

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/webcodingcat/clowder/internal/units"
)

// FileName is the name of the file in the cluster directory.
const FileName = "cluster.yaml"

// Config is cluster.yaml: everything cluster-wide. Changes take effect with
// `clowder cluster apply`; the file is the source, the cluster keeps a copy.
// No secret is in it: where cluster.yaml has a value tagged !secret, Config
// has its name (SecretRef), and the value travels apart. The json names are
// how the cluster state stores the file: a Go field may be renamed, its tags
// never.
type Config struct {
	// Name of the cluster, shown by the CLI; it finds the user's credentials
	// by it.
	Name string `yaml:"name" json:"name" jsonschema:"required"`
	// Seeds are a few addresses the CLI tries first; it asks the cluster for
	// the current members.
	Seeds  []string `yaml:"seeds" json:"seeds" jsonschema:"required,minItems=1"`
	Hoster Hoster   `yaml:"hoster" json:"hoster" jsonschema:"required"`
	// Registries are the logins for private image registries, by host.
	Registries map[string]Registry `yaml:"registries,omitempty" json:"registries,omitempty"`
	// PrivateNetwork is the hoster's private network the hosts share, as a
	// CIDR; containers may not reach it. Empty when the hosts reach each
	// other over their public addresses.
	PrivateNetwork string                  `yaml:"private_network,omitempty" json:"private_network,omitempty"`
	Ingress        Ingress                 `yaml:"ingress" json:"ingress" jsonschema:"required"`
	StorageClasses map[string]StorageClass `yaml:"storage_classes" json:"storage_classes" jsonschema:"required"`
	Replication    Replication             `yaml:"replication,omitempty" json:"replication,omitempty"`
	Priorities     Priorities              `yaml:"priorities,omitempty" json:"priorities,omitempty"`
	Stacks         Stacks                  `yaml:"stacks,omitempty" json:"stacks,omitempty"`
	Defaults       Defaults                `yaml:"defaults,omitempty" json:"defaults,omitempty"`
	SystemReserve  Resources               `yaml:"system_reserve,omitempty" json:"system_reserve,omitempty"`
	Alerts         *Alerts                 `yaml:"alerts,omitempty" json:"alerts,omitempty"`
	ACME           ACME                    `yaml:"acme,omitempty" json:"acme,omitempty"`
}

// Priorities are the categories a stack or service may name instead of a
// number. Without the block, DefaultPriorities applies.
type Priorities struct {
	// Default is the category of stacks that name none.
	Default string `yaml:"default,omitempty" json:"default,omitempty"`
	// Categories map names to numbers from 0 to 1000.
	Categories map[string]int `yaml:"categories,omitempty" json:"categories,omitempty"`
}

// Priority is a priority as a stack or service writes it: a number from 0
// to 1000 or the name of a category; the zero value means "the cluster's
// default". The cluster's categories turn it into a number (Resolve).
type Priority struct {
	Value    *int   `json:"value,omitempty"`
	Category string `json:"category,omitempty"`
}

// Resolve turns a priority into a number with the categories: a number
// stays, a category is looked up, none is the default.
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

// DefaultPriorities are the categories when cluster.yaml names none.
func DefaultPriorities() Priorities {
	return Priorities{
		Default:    "normal",
		Categories: map[string]int{"critical": 900, "high": 700, "normal": 500, "low": 200},
	}
}

// WithDefaults returns the config with what cluster.yaml may leave out filled
// in. Load does it, and so does the cluster when it applies a config.
func (c Config) WithDefaults() Config {
	c.Priorities = c.Priorities.withDefaults()
	c.Defaults.Reservations = c.Defaults.Reservations.withDefaults(DefaultReservations)
	c.SystemReserve = c.SystemReserve.withDefaults(DefaultSystemReserve)
	if c.Replication.ResyncRate == "" {
		c.Replication.ResyncRate = DefaultResyncRate
	}
	return c
}

// The resources cluster.yaml may leave out: what a service reserves without
// deploy.resources.reservations, and what a host keeps for itself (the
// system, Docker, clowderd, Traefik).
var (
	DefaultReservations  = Resources{Memory: "128M", CPUs: "0.1"}
	DefaultSystemReserve = Resources{Memory: "512M", CPUs: "0.25"}
)

// DefaultResyncRate is how fast a copy resyncs without replication.resync_rate
// (bytes per second, per resync).
const DefaultResyncRate = "50M"

// withDefaults fills in each field left out.
func (r Resources) withDefaults(defaults Resources) Resources {
	if r.Memory == "" {
		r.Memory = defaults.Memory
	}
	if r.CPUs == "" {
		r.CPUs = defaults.CPUs
	}
	return r
}

// withDefaults fills in what cluster.yaml leaves out: without categories the
// default ones, and their default unless another one is named.
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

// Hoster is the cloud the hosts run in; Type picks the plugin, like the
// types of alert channels.
type Hoster struct {
	Type  string    `yaml:"type" json:"type" jsonschema:"required,enum=hetzner"`
	Token SecretRef `yaml:"token" json:"token" jsonschema:"required"`
	// API is the full URL of the hoster's API when it is not the hoster's
	// own (a test lab).
	API string `yaml:"api,omitempty" json:"api,omitempty"`
}

// SecretRef is the name of a secret: where cluster.yaml has a value tagged
// !secret, the CLI puts the value's place there (SecretPlaces), so the name
// is the path, such as hoster.token. "" is no secret.
type SecretRef string

// Registry is the login for a private image registry; only the password is
// a secret.
type Registry struct {
	Username string    `yaml:"username" json:"username" jsonschema:"required"`
	Password SecretRef `yaml:"password" json:"password" jsonschema:"required"`
}

// Ingress is the Floating IP and the hosts that should hold it.
type Ingress struct {
	FloatingIP     string   `yaml:"floating_ip" json:"floating_ip" jsonschema:"required"`
	PreferredHosts []string `yaml:"preferred_hosts,omitempty" json:"preferred_hosts,omitempty"`
}

// StorageClass is either a thin pool on each host ("vg/thinpool") or a
// fallback list of other classes.
type StorageClass struct {
	Pool string `yaml:"pool,omitempty" json:"pool,omitempty"`
	// Reserve kept free in the pool for snapshots and metadata, e.g. "20%".
	Reserve  string   `yaml:"reserve,omitempty" json:"reserve,omitempty"`
	Fallback []string `yaml:"fallback,omitempty" json:"fallback,omitempty"`
}

// DefaultReservePercent is a pool's reserve when its class names none.
const DefaultReservePercent = 20

// ReservePercent is the class's reserve in percent of its pool; Check has
// made sure Reserve reads.
func (c StorageClass) ReservePercent() int {
	if number, err := strconv.Atoi(strings.TrimSuffix(c.Reserve, "%")); err == nil {
		return number
	}
	return DefaultReservePercent
}

// Replication limits DRBD's resync so it never crowds out the cluster's own
// traffic.
type Replication struct {
	// ResyncRate per host, e.g. "50M" (bytes per second).
	ResyncRate string `yaml:"resync_rate,omitempty" json:"resync_rate,omitempty"`
}

// Stacks holds what the cluster allows single stacks beyond the rules for
// all of them.
type Stacks struct {
	// Privileged stacks may use privileged, capabilities beyond the harmless
	// ones and security_opt: they reach everything on their host, the data
	// and secrets of other stacks included.
	Privileged []string `yaml:"privileged,omitempty" json:"privileged,omitempty"`
}

// Defaults apply to services without their own values.
type Defaults struct {
	Reservations Resources `yaml:"reservations,omitempty" json:"reservations,omitempty"`
}

// Resources are memory (e.g. "128M") and CPUs (e.g. "0.1").
type Resources struct {
	Memory string `yaml:"memory,omitempty" json:"memory,omitempty"`
	CPUs   string `yaml:"cpus,omitempty" json:"cpus,omitempty"`
}

// Alerts are the channels and the optional heartbeat.
type Alerts struct {
	Channels  []AlertChannel `yaml:"channels,omitempty" json:"channels,omitempty"`
	Heartbeat *Heartbeat     `yaml:"heartbeat,omitempty" json:"heartbeat,omitempty"`
}

// AlertChannel is one notifier: discord, email or webhook.
type AlertChannel struct {
	Type        string    `yaml:"type" json:"type" jsonschema:"required,enum=discord,enum=email,enum=webhook"`
	MinSeverity string    `yaml:"min_severity,omitempty" json:"min_severity,omitempty" jsonschema:"enum=info,enum=warning,enum=critical"`
	Webhook     SecretRef `yaml:"webhook,omitempty" json:"webhook,omitempty"`
	URL         SecretRef `yaml:"url,omitempty" json:"url,omitempty"`
	SMTP        *SMTP     `yaml:"smtp,omitempty" json:"smtp,omitempty"`
	From        string    `yaml:"from,omitempty" json:"from,omitempty"`
	To          []string  `yaml:"to,omitempty" json:"to,omitempty"`
}

// The severities of an alert, which a channel's min_severity names.
const (
	SeverityInfo     = "info"
	SeverityWarning  = "warning"
	SeverityCritical = "critical"
)

// Severities are the severities, lowest first.
var Severities = []string{SeverityInfo, SeverityWarning, SeverityCritical}

// AtLeast tells whether severity is min or above; an empty min is info.
func AtLeast(severity, min string) bool {
	if min == "" {
		min = SeverityInfo
	}
	return slices.Index(Severities, severity) >= slices.Index(Severities, min)
}

// SMTP is the mail server of an email channel. Port 465 speaks TLS from the
// start; any other port must offer STARTTLS.
type SMTP struct {
	Host     string    `yaml:"host" json:"host" jsonschema:"required"`
	Port     int       `yaml:"port" json:"port" jsonschema:"required"`
	User     string    `yaml:"user,omitempty" json:"user,omitempty"`
	Password SecretRef `yaml:"password,omitempty" json:"password,omitempty"`
	// CA is a PEM file next to cluster.yaml, written with "/", that the
	// leader trusts for the server besides the system's (a test CA).
	CA string `yaml:"ca,omitempty" json:"ca,omitempty"`
}

// Heartbeat is called by the leader at an interval; an external service
// alarms when the calls stop.
type Heartbeat struct {
	URL SecretRef `yaml:"url" json:"url" jsonschema:"required"`
	// Interval is a duration like "1m" (the default).
	Interval string `yaml:"interval,omitempty" json:"interval,omitempty"`
}

// DefaultHeartbeatInterval is how often the heartbeat is called when
// cluster.yaml names no interval.
const DefaultHeartbeatInterval = time.Minute

// Every is how often the heartbeat is called; Check made sure Interval
// reads.
func (h Heartbeat) Every() time.Duration {
	if interval, err := units.ParseDuration(h.Interval); err == nil && interval > 0 {
		return interval
	}
	return DefaultHeartbeatInterval
}

// ACME is where certificates come from; empty means Let's Encrypt.
type ACME struct {
	// Directory is the full URL of the ACME directory.
	Directory string `yaml:"directory,omitempty" json:"directory,omitempty"`
	// CA is a PEM file next to cluster.yaml, written with "/", that the
	// leader trusts for the directory besides the system's (a test CA).
	CA string `yaml:"ca,omitempty" json:"ca,omitempty"`
	// Email is the account's contact for the CA's notices.
	Email string `yaml:"email,omitempty" json:"email,omitempty"`
	// RenewBefore is how long before its end a certificate is renewed, e.g.
	// "30d"; by default when a third of its lifetime is left.
	RenewBefore string `yaml:"renew_before,omitempty" json:"renew_before,omitempty"`
}

// DefaultACMEDirectory is Let's Encrypt's.
const DefaultACMEDirectory = "https://acme-v02.api.letsencrypt.org/directory"

// DirectoryURL is the ACME directory to use.
func (a ACME) DirectoryURL() string {
	if a.Directory == "" {
		return DefaultACMEDirectory
	}
	return a.Directory
}
