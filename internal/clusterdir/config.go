package clusterdir

// Config is cluster.yaml: everything cluster-wide. Changes take effect with
// `clowder cluster apply`; the file is the source, the cluster keeps a copy.
// Secrets never appear here, only references to secrets.sops.yaml. The json
// names are how the cluster state stores the file: a Go field may be
// renamed, its tags never.
type Config struct {
	// Name of the cluster, shown by the CLI.
	Name string `yaml:"name" json:"name" jsonschema:"required"`
	// Seeds are a few addresses the CLI tries first; it asks the cluster for
	// the current members.
	Seeds []string `yaml:"seeds" json:"seeds" jsonschema:"required,minItems=1"`
	// Hoster is the hoster plugin, e.g. hetzner.
	Hoster      string    `yaml:"hoster" json:"hoster" jsonschema:"required"`
	HosterToken SecretRef `yaml:"hoster_token" json:"hoster_token" jsonschema:"required"`
	// Network the hosts use for WireGuard: private or public.
	Network        string                  `yaml:"network,omitempty" json:"network,omitempty" jsonschema:"enum=private,enum=public"`
	Ingress        Ingress                 `yaml:"ingress" json:"ingress" jsonschema:"required"`
	StorageClasses map[string]StorageClass `yaml:"storage_classes" json:"storage_classes" jsonschema:"required"`
	Replication    Replication             `yaml:"replication,omitempty" json:"replication,omitempty"`
	Priorities     Priorities              `yaml:"priorities,omitempty" json:"priorities,omitempty"`
	Defaults       Defaults                `yaml:"defaults,omitempty" json:"defaults,omitempty"`
	SystemReserve  Resources               `yaml:"system_reserve,omitempty" json:"system_reserve,omitempty"`
	Backup         *Backup                 `yaml:"backup,omitempty" json:"backup,omitempty"`
	Alerts         *Alerts                 `yaml:"alerts,omitempty" json:"alerts,omitempty"`
	ACME           ACME                    `yaml:"acme,omitempty" json:"acme,omitempty"`
	Metrics        Metrics                 `yaml:"metrics,omitempty" json:"metrics,omitempty"`
}

// Priorities are the categories a stack or service may name instead of a
// number. Without the block, DefaultPriorities applies.
type Priorities struct {
	// Default is the category of stacks that name none.
	Default string `yaml:"default,omitempty" json:"default,omitempty"`
	// Categories map names to numbers from 0 to 1000.
	Categories map[string]int `yaml:"categories,omitempty" json:"categories,omitempty"`
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
	return c
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

// SecretRef points to a value in secrets.sops.yaml.
type SecretRef struct {
	Secret string `yaml:"secret" json:"secret" jsonschema:"required"`
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

// Replication limits DRBD's resync so it never crowds out the cluster's own
// traffic.
type Replication struct {
	// ResyncRate per host, e.g. "50M" (bytes per second).
	ResyncRate string `yaml:"resync_rate,omitempty" json:"resync_rate,omitempty"`
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

// Backup is where restic writes.
type Backup struct {
	Target   BackupTarget `yaml:"target" json:"target" jsonschema:"required"`
	Password SecretRef    `yaml:"password" json:"password" jsonschema:"required"`
}

// BackupTarget is a restic repository.
type BackupTarget struct {
	Type string `yaml:"type" json:"type" jsonschema:"required,enum=restic"`
	URL  string `yaml:"url" json:"url" jsonschema:"required"`
}

// Alerts are the channels and the optional heartbeat.
type Alerts struct {
	Channels  []AlertChannel `yaml:"channels,omitempty" json:"channels,omitempty"`
	Heartbeat *Heartbeat     `yaml:"heartbeat,omitempty" json:"heartbeat,omitempty"`
}

// AlertChannel is one notifier: discord, email or webhook.
type AlertChannel struct {
	Type        string     `yaml:"type" json:"type" jsonschema:"required,enum=discord,enum=email,enum=webhook"`
	MinSeverity string     `yaml:"min_severity,omitempty" json:"min_severity,omitempty" jsonschema:"enum=info,enum=warning,enum=critical"`
	Webhook     *SecretRef `yaml:"webhook,omitempty" json:"webhook,omitempty"`
	URL         *SecretRef `yaml:"url,omitempty" json:"url,omitempty"`
	SMTP        *SMTP      `yaml:"smtp,omitempty" json:"smtp,omitempty"`
	From        string     `yaml:"from,omitempty" json:"from,omitempty"`
	To          []string   `yaml:"to,omitempty" json:"to,omitempty"`
}

// SMTP is the mail server of an email channel.
type SMTP struct {
	Host     string     `yaml:"host" json:"host" jsonschema:"required"`
	Port     int        `yaml:"port" json:"port" jsonschema:"required"`
	User     string     `yaml:"user,omitempty" json:"user,omitempty"`
	Password *SecretRef `yaml:"password,omitempty" json:"password,omitempty"`
}

// Heartbeat is called by the leader at an interval; an external service
// alarms when the calls stop.
type Heartbeat struct {
	URL      SecretRef `yaml:"url" json:"url" jsonschema:"required"`
	Interval string    `yaml:"interval,omitempty" json:"interval,omitempty"`
}

// ACME is where certificates come from; empty means Let's Encrypt.
type ACME struct {
	Directory string `yaml:"directory,omitempty" json:"directory,omitempty"`
	// CA is a PEM file next to cluster.yaml to trust for the directory (tests).
	CA    string `yaml:"ca,omitempty" json:"ca,omitempty"`
	Email string `yaml:"email,omitempty" json:"email,omitempty"`
}

// Metrics configures /metrics.
type Metrics struct {
	// Listen is "node" (default) or "node+underlay".
	Listen string `yaml:"listen,omitempty" json:"listen,omitempty" jsonschema:"enum=node,enum=node+underlay"`
}
