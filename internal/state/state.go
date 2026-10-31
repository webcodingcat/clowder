// Package state is the cluster's replicated state and the commands that
// change it. Every agent holds the same State and applies the same commands
// in the same order (Raft's log), so Apply must be deterministic: it does no
// I/O, never reads the clock (commands carry their time) and never uses
// randomness. Secrets arrive sealed; this package never sees them in clear.
package state

import (
	"maps"
	"net/netip"
	"slices"
	"time"

	"github.com/webcodingcat/clowder/internal/buildinfo"
	"github.com/webcodingcat/clowder/internal/clusterspec"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/resource"
	"github.com/webcodingcat/clowder/internal/stackspec"
)

// MaxHosts is the size of a cluster: a host ID is also the DRBD node-id and
// the last part of the node address and the WireGuard port (51800 + ID;
// stackspec.ReservedPort keeps 51800-51831 free, so both change together).
const MaxHosts = 32

// HostID is a host's number, 0 to MaxHosts-1.
type HostID int

// Sealed is a secret encrypted with age for the cluster's data key.
type Sealed []byte

// State is everything the cluster agrees on. The zero value is an empty
// cluster that only accepts the bootstrap command.
type State struct {
	// Index is the Raft index of the last applied command; reads that need
	// a write to be visible wait for it.
	Index  uint64        `json:"index"`
	Keys   *ClusterKeys  `json:"keys,omitempty"`
	Config ClusterConfig `json:"config"`
	// ClusterSecrets are the secrets cluster.yaml names (the hoster's
	// token, the alert channels'), by name.
	ClusterSecrets map[string]Sealed `json:"cluster_secrets,omitempty"`
	JoinKeys       []Sealed          `json:"join_keys,omitempty"`
	Registries     map[string]Sealed `json:"registries,omitempty"`
	Hosts          map[HostID]*Host  `json:"hosts,omitempty"`
	// Ingress is the host that should hold the Floating IP; the leader keeps
	// the hoster's routing in line with it.
	Ingress *HostID `json:"ingress,omitempty"`
	// ACMEAccount is the cluster's account at the ACME directory; the
	// leader makes it at its first order.
	ACMEAccount *ACMEAccount `json:"acme_account,omitempty"`
	// Challenges are the HTTP-01 answers of the orders in progress: token
	// -> key authorization. Every agent serves them.
	Challenges map[string]string `json:"challenges,omitempty"`
	// Certificates are the HTTP routes' certificates by domain.
	Certificates map[string]*Certificate `json:"certificates,omitempty"`
	Users        map[string]*User        `json:"users,omitempty"`
	Revoked      map[string]time.Time    `json:"revoked,omitempty"` // certificate serial -> when
	// Namespaces hold the other resources; the bootstrap makes names.Default.
	Namespaces map[string]*Namespace `json:"namespaces,omitempty"`
	Stacks     map[string]*Stack     `json:"stacks,omitempty"`  // names.ID: "<namespace>/<stack>"
	Secrets    map[string]*Secret    `json:"secrets,omitempty"` // names.ID: "<namespace>/<secret>"
	Volumes    map[string]*Volume    `json:"volumes,omitempty"` // names.ID: "<namespace>/<volume>"
	// Retired are the volume copies the cluster gave up: the only ones an
	// agent removes, so a copy that is merely missing from the state (a bug)
	// keeps its data.
	Retired []RetiredCopy `json:"retired,omitempty"`
	// Alerts are the open alerts by key.
	Alerts     map[string]*Alert     `json:"alerts,omitempty"`
	Operations map[string]*Operation `json:"operations,omitempty"`
	Locks      map[string]string     `json:"locks,omitempty"` // "stack/<stack ID>" or "volume/<volume ID>" -> operation
	Audit      []AuditEntry          `json:"audit,omitempty"`
	// changes are the resources the command being applied changed (Apply
	// returns them); not part of the state.
	changes []Change
}

// ClusterKeys are the keys init creates. The CA certificate is public; its
// key and the cluster's age identity (for SOPS files in GitOps sources) are
// sealed.
type ClusterKeys struct {
	CACertificate []byte `json:"ca_certificate"`
	CAKey         Sealed `json:"ca_key"`
	AgeIdentity   Sealed `json:"age_identity"`
}

// ClusterConfig is the applied cluster.yaml.
type ClusterConfig struct {
	// Revision counts the applies, starting at 1.
	Revision int64 `json:"revision"`
	// Hash of cluster.yaml and the secrets as the CLI applied them, so diff
	// can tell whether anything changed.
	Hash string             `json:"hash,omitempty"`
	Spec clusterspec.Config `json:"spec"`
	// Files are the files next to cluster.yaml that it names (acme.ca).
	Files map[string][]byte `json:"files,omitempty"`
}

// HostPhase is where a host is in its life; see phaseChanges for the moves.
type HostPhase string

// The phases of a host.
const (
	Joining    HostPhase = "joining"
	Passive    HostPhase = "passive"
	Active     HostPhase = "active"
	Draining   HostPhase = "draining"
	Failed     HostPhase = "failed"
	Fenced     HostPhase = "fenced"
	Recovering HostPhase = "recovering"
	Disabled   HostPhase = "disabled"
	Removed    HostPhase = "removed"
)

// Phases are all phases of a host, in the order of a host's life.
var Phases = []HostPhase{Joining, Passive, Active, Draining, Failed, Fenced, Recovering, Disabled, Removed}

// Host is one member of the cluster.
type Host struct {
	ID        HostID     `json:"id"`
	Name      string     `json:"name"`
	ServerRef string     `json:"server_ref,omitempty"` // the hoster's ID of the server
	Underlay  netip.Addr `json:"underlay"`
	// PublicAddress is where users reach the host's API when the underlay is
	// a private network; the CLI learns it from the cluster.
	PublicAddress netip.Addr `json:"public_address,omitzero"`
	WGPublicKey   string     `json:"wg_public_key"`
	// CertificateSerial is the host certificate; removing the host revokes it,
	// so a removed host leaves Raft and the API at once.
	CertificateSerial string    `json:"certificate_serial"`
	Voter             bool      `json:"voter"`
	Phase             HostPhase `json:"phase"`
	Capacity          Resources `json:"capacity"`
	// Pools are the host's thin pools by name ("vg/thin"), as it reported
	// them; storage classes name pools.
	Pools map[string]Pool `json:"pools,omitempty"`
	// Version of the agent, as it last reported it.
	Version string `json:"version,omitempty"`
	// DockerDown is true when Docker did not answer the host's last ping.
	DockerDown bool `json:"docker_down,omitempty"`
	// RootUsedPercent is how full the root file system is, in whole percent.
	RootUsedPercent int `json:"root_used_percent,omitempty"`
}

// Resources are a host's memory and CPUs, as it reports them; the
// scheduler takes system_reserve off.
type Resources struct {
	MemoryBytes int64   `json:"memory_bytes"`
	CPUs        float64 `json:"cpus"`
}

// Pool is a thin pool's size and how full its data and metadata are. Whole
// percent rather than bytes: the same threshold works for any pool size,
// and a report changes only when a value moves by a percent.
type Pool struct {
	Name            string `json:"name"` // volume-group/thin-pool
	SizeBytes       int64  `json:"size_bytes"`
	DataPercent     int    `json:"data_percent"`
	MetadataPercent int    `json:"metadata_percent"`
}

// User may use the CLI with the certificates listed here.
type User struct {
	Name string `json:"name"`
	// Serials of the user's certificates; removing the user revokes them.
	Serials []string `json:"serials,omitempty"`
}

// Meta is what every resource has besides its spec and status.
type Meta struct {
	// Namespace is empty for a namespace, which lives in none.
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	// Owner is the channel that applied the resource (OwnerCLI or
	// GitOpsOwner): only it changes or deletes the resource. names.Default
	// belongs to no channel.
	Owner string `json:"owner,omitempty"`
	// Generation counts the changes of the spec.
	Generation int64 `json:"generation"`
	// ResourceVersion is the Raft index of the command that last changed the
	// resource, its spec or its status.
	ResourceVersion uint64 `json:"resource_version"`
}

// Namespace keeps its resources apart from those of other namespaces: two
// namespaces may each have a stack, volume or secret of the same name.
type Namespace struct {
	Meta
}

// Secret is a secret of a namespace, by names.ID in State.Secrets.
type Secret struct {
	Meta
	Spec SealedSecret `json:"spec"`
}

// SealedSecret is a secret's spec as the state keeps it: the value sealed,
// so the state never sees it in clear, and its digest, by which an apply of
// the same value is told from a change (sealing the same value twice gives
// two different results).
type SealedSecret struct {
	Value Sealed `json:"value"`
	// Digest is an HMAC over the secret's ID and value, which the API
	// computes with a key derived from the data key.
	Digest string `json:"digest"`
}

// Stack is a stack of a namespace: its spec as the user applied it,
// normalized, and its status, which the leader and its host write.
type Stack struct {
	Meta
	Spec stackspec.Spec `json:"spec"`
	// Services are what the scheduler needs of each service, by name,
	// worked out from the spec when it was put (servicesOf), so the
	// scheduler reads no sizes or priorities again.
	Services map[string]Service `json:"services,omitempty"`
	Host     *HostID            `json:"host,omitempty"` // nil: not placed yet
	Status   StackStatus        `json:"status"`
	// AppliedSecrets are the digests of the secrets the containers on its
	// host came up with, by name: a changed secret that has not reached the
	// containers yet differs from its digest.
	AppliedSecrets map[string]string `json:"applied_secrets,omitempty"`
	// PushedOut are the services stopped for services of higher priority,
	// sorted; only the leader changes the list (set-pushed-out).
	PushedOut []string `json:"pushed_out,omitempty"`
	// Restarted is when a user last restarted each service (restart-stack),
	// by name: its host recreates a service whose time changed.
	Restarted map[string]time.Time `json:"restarted,omitempty"`
	// LastMove is when the stack last got another host. Nothing reads it
	// yet: failover (phase 2) uses it to keep a stack from moving back and
	// forth.
	LastMove time.Time `json:"last_move,omitzero"`
	// WaitReason is why the placement cannot place the stack or a volume it
	// newly names; empty while nothing waits. Display only.
	WaitReason string `json:"wait_reason,omitempty"`
	// WaitingSince is when the stack began to wait: kept while only the
	// reason's text changes. It is in the state, not the leader's memory,
	// so how long a stack has waited survives a change of leader; the
	// alert for a stack that waits too long reads it.
	WaitingSince time.Time `json:"waiting_since,omitzero"`
}

// ID is the stack's key in State.Stacks.
func (s *Stack) ID() string {
	return names.ID(s.Namespace, s.Name)
}

// Project is the stack's Compose project on its host.
func (s *Stack) Project() string {
	return names.Project(s.Namespace, s.Name)
}

// Service is what the scheduler needs of one service of a stack.
type Service struct {
	// Priority is as written: the service's own, else the stack's; the
	// scheduler resolves it with the current cluster.yaml, so a changed
	// category takes effect without a new apply.
	Priority clusterspec.Priority `json:"priority,omitzero"`
	// MemoryBytes and CPUs are the reservations; zero means
	// defaults.reservations.
	MemoryBytes int64   `json:"memory_bytes,omitempty"`
	CPUs        float64 `json:"cpus,omitempty"`
	// DependsOn are the services of the stack it needs.
	DependsOn []string `json:"depends_on,omitempty"`
}

// OwnerCLI owns the resources users apply with the CLI.
const OwnerCLI = "cli"

// GitOpsOwner owns the resources of a GitOps source; a resource has one
// owner, so the CLI and a source never overwrite each other.
func GitOpsOwner(source string) string {
	return "gitops:" + source
}

// StackStatus says what the stack is doing.
type StackStatus string

// The states of a stack.
const (
	StackPending  StackStatus = "pending"
	StackStarting StackStatus = "starting"
	StackRunning  StackStatus = "running"
	// not set yet: failover (phase 2) waits for a host to come back, and a
	// stack is stopped by hand with operations to come
	StackWaitingForHost StackStatus = "waiting-for-host"
	StackStopped        StackStatus = "stopped"
)

// Volume is a volume of a namespace and where its copies are.
type Volume struct {
	Meta
	Spec resource.VolumeSpec `json:"spec"`
	// DataGeneration grows with a restore or a class move; it is part of
	// the LV's name (names.VolumeName), at most names.MaxDataGeneration.
	DataGeneration int64        `json:"data_generation"`
	Minor          *int         `json:"minor,omitempty"` // DRBD minor; nil with replicas 0
	Copies         []VolumeCopy `json:"copies,omitempty"`
	Status         VolumeStatus `json:"status"`
}

// ID is the volume's key in State.Volumes.
func (v *Volume) ID() string {
	return names.ID(v.Namespace, v.Name)
}

// VolumeCopy is one copy of a volume, on one host.
type VolumeCopy struct {
	Host HostID `json:"host"`
	// Class is the pool's class the copy is in (any falls back to another).
	Class      string `json:"class"`
	Tiebreaker bool   `json:"tiebreaker,omitempty"` // diskless
}

// RetiredCopy is a copy of a volume that its host may remove: of a deleted
// stack or volume, of an older generation, or one the volume no longer
// places there. The host forgets it once the copy is gone (forget-copy).
type RetiredCopy struct {
	Volume         string `json:"volume"` // the volume's ID
	DataGeneration int64  `json:"data_generation"`
	Host           HostID `json:"host"`
}

// VolumeStatus says how healthy a volume's copies are.
type VolumeStatus string

// The states of a volume.
const (
	// VolumePending has no copy yet: a volume is placed together with the
	// first stack that names it.
	VolumePending VolumeStatus = "pending"
	VolumeOK      VolumeStatus = "ok"
	// not set yet: replicas on several hosts come with phase 2, restores
	// with phase 3
	VolumeDegraded       VolumeStatus = "degraded"
	VolumeResyncing      VolumeStatus = "resyncing"
	VolumeRestorePending VolumeStatus = "restore-pending"
)

// AuditEntry records one applied command.
type AuditEntry struct {
	Index uint64    `json:"index"`
	At    time.Time `json:"at"`
	Actor string    `json:"actor"`
	Kind  string    `json:"kind"`
}

// auditSize is how many audit entries the state keeps.
const auditSize = 10000

// MinVersion is the oldest agent version in the cluster (hosts that have not
// reported one count as unknown and are skipped); the leader uses new
// command kinds only when every agent knows them.
func (s *State) MinVersion() string {
	var oldest string
	for _, id := range slices.Sorted(maps.Keys(s.Hosts)) {
		host := s.Hosts[id]
		if host.Phase == Removed || host.Version == "" {
			continue
		}
		if oldest == "" || buildinfo.Compare(host.Version, oldest) < 0 {
			oldest = host.Version
		}
	}
	return oldest
}
