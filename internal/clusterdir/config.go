package clusterdir

// Config is cluster.yaml: everything cluster-wide. Changes take effect with
// `clowder cluster apply`; the file is the source, the cluster keeps a copy.
// Secrets never appear here, only references to secrets.sops.yaml.
type Config struct {
	// Name of the cluster, shown by the CLI.
	Name string `yaml:"name" jsonschema:"required"`
	// Seeds are a few addresses the CLI tries first; it asks the cluster for
	// the current members.
	Seeds []string `yaml:"seeds" jsonschema:"required,minItems=1"`
	// Hoster is the hoster plugin, e.g. hetzner.
	Hoster      string    `yaml:"hoster" jsonschema:"required"`
	HosterToken SecretRef `yaml:"hoster_token"`
	// Network the hosts use for WireGuard: private or public.
	Network        string                  `yaml:"network,omitempty" jsonschema:"enum=private,enum=public"`
	Ingress        Ingress                 `yaml:"ingress" jsonschema:"required"`
	StorageClasses map[string]StorageClass `yaml:"storage_classes" jsonschema:"required"`
	Replication    Replication             `yaml:"replication,omitempty"`
	Priorities     Priorities              `yaml:"priorities,omitempty"`
	Defaults       Defaults                `yaml:"defaults,omitempty"`
	SystemReserve  Resources               `yaml:"system_reserve,omitempty"`
	Backup         *Backup                 `yaml:"backup,omitempty"`
	Alerts         *Alerts                 `yaml:"alerts,omitempty"`
	ACME           ACME                    `yaml:"acme,omitempty"`
	Metrics        Metrics                 `yaml:"metrics,omitempty"`
}

// Priorities are the categories a stack or service may name instead of a
// number. Without the block, DefaultPriorities applies.
type Priorities struct {
	// Default is the category of stacks that name none.
	Default string `yaml:"default,omitempty"`
	// Categories map names to numbers from 0 to 1000.
	Categories map[string]int `yaml:"categories,omitempty"`
}

// DefaultPriorities are the categories when cluster.yaml names none.
func DefaultPriorities() Priorities {
	return Priorities{
		Default:    "normal",
		Categories: map[string]int{"critical": 900, "high": 700, "normal": 500, "low": 200},
	}
}

// SecretRef points to a value in secrets.sops.yaml.
type SecretRef struct {
	Secret string `yaml:"secret" jsonschema:"required"`
}

// Ingress is the Floating IP and the hosts that should hold it.
type Ingress struct {
	FloatingIP     string   `yaml:"floating_ip" jsonschema:"required"`
	PreferredHosts []string `yaml:"preferred_hosts,omitempty"`
}

// StorageClass is either a thin pool on each host ("vg/thinpool") or a
// fallback list of other classes.
type StorageClass struct {
	Pool string `yaml:"pool,omitempty"`
	// Reserve kept free in the pool for snapshots and metadata, e.g. "20%".
	Reserve  string   `yaml:"reserve,omitempty"`
	Fallback []string `yaml:"fallback,omitempty"`
}

// Replication limits DRBD's resync so it never crowds out the cluster's own
// traffic.
type Replication struct {
	// ResyncRate per host, e.g. "50M" (bytes per second).
	ResyncRate string `yaml:"resync_rate,omitempty"`
}

// Defaults apply to services without their own values.
type Defaults struct {
	Reservations Resources `yaml:"reservations,omitempty"`
}

// Resources are memory (e.g. "128M") and CPUs (e.g. "0.1").
type Resources struct {
	Memory string `yaml:"memory,omitempty"`
	CPUs   string `yaml:"cpus,omitempty"`
}

// Backup is where restic writes.
type Backup struct {
	Target   BackupTarget `yaml:"target" jsonschema:"required"`
	Password SecretRef    `yaml:"password" jsonschema:"required"`
}

// BackupTarget is a restic repository.
type BackupTarget struct {
	Type string `yaml:"type" jsonschema:"required"`
	URL  string `yaml:"url" jsonschema:"required"`
}

// Alerts are the channels and the optional heartbeat.
type Alerts struct {
	Channels  []AlertChannel `yaml:"channels,omitempty"`
	Heartbeat *Heartbeat     `yaml:"heartbeat,omitempty"`
}

// AlertChannel is one notifier: discord, email or webhook.
type AlertChannel struct {
	Type        string     `yaml:"type" jsonschema:"required,enum=discord,enum=email,enum=webhook"`
	MinSeverity string     `yaml:"min_severity,omitempty" jsonschema:"enum=info,enum=warning,enum=critical"`
	Webhook     *SecretRef `yaml:"webhook,omitempty"`
	URL         *SecretRef `yaml:"url,omitempty"`
	SMTP        *SMTP      `yaml:"smtp,omitempty"`
	From        string     `yaml:"from,omitempty"`
	To          []string   `yaml:"to,omitempty"`
}

// SMTP is the mail server of an email channel.
type SMTP struct {
	Host     string     `yaml:"host" jsonschema:"required"`
	Port     int        `yaml:"port" jsonschema:"required"`
	User     string     `yaml:"user,omitempty"`
	Password *SecretRef `yaml:"password,omitempty"`
}

// Heartbeat is called by the leader at an interval; an external service
// alarms when the calls stop.
type Heartbeat struct {
	URL      SecretRef `yaml:"url" jsonschema:"required"`
	Interval string    `yaml:"interval,omitempty"`
}

// ACME is where certificates come from; empty means Let's Encrypt.
type ACME struct {
	Directory string `yaml:"directory,omitempty"`
	// CA is a PEM file next to cluster.yaml to trust for the directory (tests).
	CA    string `yaml:"ca,omitempty"`
	Email string `yaml:"email,omitempty"`
}

// Metrics configures /metrics.
type Metrics struct {
	// Listen is "node" (default) or "node+underlay".
	Listen string `yaml:"listen,omitempty" jsonschema:"enum=node,enum=node+underlay"`
}
