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

	"github.com/webcodingcat/clowder/internal/clusterspec"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/resource"
	"github.com/webcodingcat/clowder/internal/stackspec"
)

// MaxHosts is the size of a cluster. A host ID is also the DRBD node-id,
// the last part of the node address, and part of the WireGuard port
// (51800 + ID). stackspec.ReservedPort keeps 51800-51831 free, so both
// constants change together.
const MaxHosts = 32

// HostID is a host's number, 0 to MaxHosts-1.
type HostID int

// Sealed is a secret encrypted with age for the cluster's data key.
type Sealed []byte

// State is everything the cluster agrees on. The zero value is an empty
// cluster that only accepts the bootstrap command.
type State struct {
	// Index is the Raft index of the last applied command. Reads that need
	// a write to be visible wait until Index reaches it.
	Index uint64       `json:"index"`
	Keys  *ClusterKeys `json:"keys,omitempty"`
	// Config is the ClusterConfiguration. Its generation is 0 until the
	// first configuration is applied.
	Config ClusterConfig `json:"config"`
	// ClusterName is the name that clowderd init gave the cluster, like
	// kubeadm's clusterName. cluster status shows it, and alerts and new
	// credentials carry it. Nothing checks it.
	ClusterName string `json:"cluster_name"`
	// JoinTokens are the tokens that a host may join with, by ID. Each
	// stays until a host joins with it or it expires.
	JoinTokens map[string]JoinToken `json:"join_tokens,omitempty"`
	// RecoveryRecipient is the public half of the recovery key. Every
	// encrypted value is also encrypted for it, so its holder can read the
	// values when the cluster is lost.
	RecoveryRecipient string           `json:"recovery_recipient,omitempty"`
	Hosts             map[HostID]*Host `json:"hosts,omitempty"`
	// HA is set once MinHAHosts hosts ran stacks at the same time. From
	// then on, the cluster stays highly available (checkRemoval).
	HA bool `json:"ha,omitempty"`
	// RecoveredAt is when clowderd recover left this cluster with one host
	// (recover-remap). It is zero again once the cluster is highly
	// available.
	RecoveredAt time.Time `json:"recovered_at,omitzero"`
	// Forgetting are the removed hosts whose IDs are not free yet, each
	// with the hosts that still have to forget it as a DRBD peer.
	Forgetting map[HostID][]HostID `json:"forgetting,omitempty"`
	// Ingress is the host that should hold the Floating IP; the leader keeps
	// Hetzner's routing in line with it.
	Ingress *HostID `json:"ingress,omitempty"`
	// ACMEAccount is the cluster's account at the ACME directory; the
	// leader makes it at its first order.
	ACMEAccount *ACMEAccount `json:"acme_account,omitempty"`
	// Challenges are the HTTP-01 answers of the orders in progress: token
	// -> key authorization. Every agent serves them.
	Challenges map[string]string `json:"challenges,omitempty"`
	// Certificates are the HTTP routes' certificates by domain.
	Certificates map[string]*Certificate `json:"certificates,omitempty"`
	// Revoked are the revoked host certificates by serial. Each stays until
	// a while after it has expired (revoke).
	Revoked map[string]Revocation `json:"revoked,omitempty"`
	// CLICutoff refuses the CLI certificates issued before it
	// (SetCLICutoff). Like kubeadm, the state does not list the CLI's
	// certificates one by one.
	CLICutoff time.Time `json:"cli_cutoff,omitzero"`
	// Namespaces hold the other resources; the bootstrap makes names.Default.
	Namespaces map[string]*Namespace `json:"namespaces,omitempty"`
	Stacks     map[string]*Stack     `json:"stacks,omitempty"`  // names.ID: "<namespace>/<stack>"
	Secrets    map[string]*Secret    `json:"secrets,omitempty"` // names.ID: "<namespace>/<secret>"
	Volumes    map[string]*Volume    `json:"volumes,omitempty"` // names.ID: "<namespace>/<volume>"
	// Retired are the volume copies that the cluster gave up. An agent
	// removes only these, so a copy that is merely missing from the state
	// (a bug) keeps its data.
	Retired []RetiredCopy `json:"retired,omitempty"`
	// RetiredMinors are the DRBD minors of volumes that still have a retired
	// copy, with the volume's ID. A host keeps the copy's resource, with its
	// minor, until it has removed the copy, and a new volume with the same
	// minor would clash with it there (drbdadm then refuses every command).
	// retire takes a minor, and forgetRetiredMinors gives it back.
	RetiredMinors map[int]string `json:"retired_minors,omitempty"`
	// Alerts are the open alerts by key.
	Alerts map[string]*Alert `json:"alerts,omitempty"`
	// Operations are the running operations and the last keptFinished
	// finished ones, by ID.
	Operations map[uint64]*Operation `json:"operations,omitempty"`
	// Locks are what running operations hold: lock -> operation ID.
	Locks map[string]uint64 `json:"locks,omitempty"`
	Audit []AuditEntry      `json:"audit,omitempty"`
	// changes are the resources that the command being applied changed
	// (Apply returns them). They are not part of the state.
	changes []Change
}

// ClusterKeys are the keys that init creates. The CA certificate is public.
// The CA's key and the cluster's age identity (which every value tagged
// !secret is encrypted for) are sealed.
type ClusterKeys struct {
	CACertificate []byte `json:"ca_certificate"`
	CAKey         Sealed `json:"ca_key"`
	AgeIdentity   Sealed `json:"age_identity"`
}

// ClusterConfig is the applied ClusterConfiguration, with the defaults
// filled in to its spec. Until the first apply, its generation is 0 and its
// spec is empty: no Hetzner token, no ACME, no alerts.
type ClusterConfig struct {
	Meta
	Spec clusterspec.Config `json:"spec"`
}

// HostPhase is where a host is in its life; see phaseChanges for the moves.
type HostPhase string

// The phases of a host. Failed, fencing, fenced, recovering and disabled
// belong to the failover (fencing.go in the controller).
const (
	Joining    HostPhase = "joining"
	Passive    HostPhase = "passive"
	Active     HostPhase = "active"
	Draining   HostPhase = "draining"
	Failed     HostPhase = "failed"
	Fencing    HostPhase = "fencing"
	Fenced     HostPhase = "fenced"
	Recovering HostPhase = "recovering"
	Disabled   HostPhase = "disabled"
	Removed    HostPhase = "removed"
)

// Phases are all phases of a host, in the order of a host's life.
var Phases = []HostPhase{Joining, Passive, Active, Draining, Failed, Fencing, Fenced, Recovering, Disabled, Removed}

// Host is one member of the cluster.
type Host struct {
	ID   HostID `json:"id"`
	Name string `json:"name"`
	// ServerID is the Hetzner Cloud server the host runs on.
	ServerID int64 `json:"server_id"`
	// ServerChecked says that the leader asked Hetzner and confirmed that
	// the server has the host's addresses (hetzner.Client.CheckServer).
	// Only then does the Floating IP go to the host, and only then may the
	// host be fenced, because a host names its server itself.
	ServerChecked bool `json:"server_checked,omitempty"`
	// Underlay is the host's address in the Hetzner private network.
	Underlay netip.Addr `json:"underlay"`
	// PublicAddress is the server's public IPv4, where users reach the
	// host's API.
	PublicAddress netip.Addr `json:"public_address"`
	WGPublicKey   string     `json:"wg_public_key"`
	// CertificateSerial is the serial of the host certificate. Removing the
	// host revokes the certificate, so a removed host leaves Raft and the
	// API at once.
	CertificateSerial   string    `json:"certificate_serial"`
	CertificateNotAfter time.Time `json:"certificate_not_after"`
	Voter               bool      `json:"voter"`
	Phase               HostPhase `json:"phase"`
	Capacity            Resources `json:"capacity"`
	// Pools are the host's thin pools by name ("vg/thin"), as the host
	// reported them. Storage classes name pools.
	Pools map[string]Pool `json:"pools,omitempty"`
	// Version of the agent, as it last reported it.
	Version string `json:"version,omitempty"`
	// DockerDown is true when Docker did not answer the host's last ping.
	DockerDown bool `json:"docker_down,omitempty"`
	// RootUsedPercent is how full the root file system is, in whole percent.
	RootUsedPercent int `json:"root_used_percent,omitempty"`
	// SplitBrain are the volumes, by ID, whose copy on this host is cut off
	// from a peer (a split brain that DRBD could not resolve). NoQuorum are
	// the volumes whose primary copy on this host lacks DRBD's quorum, so
	// the writes of its stack hang. Both are sorted and come from the
	// host's report.
	SplitBrain []string `json:"split_brain,omitempty"`
	NoQuorum   []string `json:"no_quorum,omitempty"`
	// Leaving marks a host that a user removes while it still has stacks or
	// volume copies (SetHostPhase.Leaving). The host drains, the placement
	// also moves its copies off, and the leader removes the host once
	// nothing is left on it.
	Leaving bool `json:"leaving,omitempty"`
	// FencedAt are the times the host was fenced within FlapWindow, oldest
	// first, for the flapping brake (Flapping). A user who enables the host
	// clears them.
	FencedAt []time.Time `json:"fenced_at,omitempty"`
}

// Resources are a host's memory and CPUs, as the host reports them. The
// scheduler subtracts systemReserve.
type Resources struct {
	MemoryBytes int64   `json:"memory_bytes"`
	CPUs        float64 `json:"cpus"`
}

// Pool is a thin pool's size and how full its data and metadata are. The
// fill levels are whole percent rather than bytes, so the same threshold
// works for any pool size, and a report changes only when a value moves by
// a percent.
type Pool struct {
	Name            string `json:"name"` // volume-group/thin-pool
	SizeBytes       int64  `json:"size_bytes"`
	DataPercent     int    `json:"data_percent"`
	MetadataPercent int    `json:"metadata_percent"`
}

// Revocation is a revoked certificate: when it was revoked, and when it
// expires.
type Revocation struct {
	At       time.Time `json:"at"`
	NotAfter time.Time `json:"not_after"`
}

// Meta is what every resource has besides its spec and status.
type Meta struct {
	// Namespace is empty for a namespace, which lives in none.
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	// Owner is the channel that applied the resource (so far always
	// OwnerCLI). Only that channel changes or deletes the resource.
	// names.Default and names.System belong to no channel.
	Owner string `json:"owner,omitempty"`
	// Generation counts the changes of the spec.
	Generation int64 `json:"generation"`
	// ResourceVersion is the Raft index of the command that last changed the
	// resource's spec or status.
	ResourceVersion uint64 `json:"resource_version"`
}

// Namespace keeps its resources apart from those of other namespaces. Two
// namespaces may each have a stack, volume or secret of the same name.
type Namespace struct {
	Meta
	Spec resource.NamespaceSpec `json:"spec,omitzero"`
}

// Secret is a secret of a namespace, by names.ID in State.Secrets.
type Secret struct {
	Meta
	Spec SealedSecret `json:"spec"`
}

// SealedSecret is a secret's spec as the state keeps it. Its values are
// sealed, by key, so the state never sees them in clear. Its digest tells
// an apply of the same values apart from a change, because sealing the
// same value twice gives two different results.
type SealedSecret struct {
	Data map[string]Sealed `json:"data"`
	// Digest is an HMAC over the secret's ID and its keys and values, which
	// the API computes with a key derived from the data key.
	Digest string `json:"digest"`
	// KeyDigests are an HMAC of each value with its key and the secret's
	// ID, by key, so a diff can tell which value changed.
	KeyDigests map[string]string `json:"key_digests"`
}

// Keys are the secret's keys, sorted.
func (s SealedSecret) Keys() []string {
	return slices.Sorted(maps.Keys(s.Data))
}

// Stack is a stack of a namespace: its spec as the user applied it, in
// normalized form, and its status, which the leader and its host write.
type Stack struct {
	Meta
	Spec stackspec.Spec `json:"spec"`
	// Services hold what the scheduler needs of each service, by name.
	// They are worked out from the spec when it is put (servicesOf), so the
	// scheduler does not read sizes or priorities again.
	Services map[string]Service `json:"services,omitempty"`
	Host     *HostID            `json:"host,omitempty"` // nil: not placed yet
	Status   StackStatus        `json:"status"`
	// UpError is why the host could not bring the stack up, while the stack
	// is failing: the first line of the first error, shortened. It is only
	// for display.
	UpError string `json:"up_error,omitempty"`
	// AppliedSecrets are the digests of the secrets that the containers on
	// the stack's host came up with, by name. A changed secret that has not
	// reached the containers yet differs from its digest here.
	AppliedSecrets map[string]string `json:"applied_secrets,omitempty"`
	// PushedOut are the services stopped for services of higher priority,
	// sorted. Only the leader changes the list (set-pushed-out).
	PushedOut []string `json:"pushed_out,omitempty"`
	// Restarted is when a user last restarted each service (restart-stack),
	// by name. The stack's host recreates a service whose time changed.
	Restarted map[string]time.Time `json:"restarted,omitempty"`
	// WaitReason is why the placement cannot place the stack or a volume
	// that it newly names. It is empty while nothing waits, and only for
	// display.
	WaitReason string `json:"wait_reason,omitempty"`
	// WaitingSince is when the stack began to wait. It is kept while only
	// the reason's text changes. It is in the state, not in the leader's
	// memory, so how long a stack has waited survives a change of leader.
	// The alert for a stack that waits too long reads it.
	WaitingSince time.Time `json:"waiting_since,omitzero"`
	// MoveTo is the host a user asked the stack to move to (request-move).
	// The placement starts the move. The end of the move clears it,
	// whether the move worked or not.
	MoveTo *HostID `json:"move_to,omitempty"`
	// LastMove is when an operation last moved the stack to another host.
	// Within a cooldown, the placement does not move it again to make room.
	LastMove time.Time `json:"last_move,omitzero"`
	// MoveFailed is when a move of the stack last ended as failed. Within
	// the cooldown, the placement starts no move of its own again (a user's
	// request still goes ahead).
	MoveFailed time.Time `json:"move_failed,omitzero"`
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
	// Priority is the priority as written: the service's own, or else the
	// stack's. The scheduler resolves it with the current configuration, so
	// a changed category takes effect without a new apply.
	Priority clusterspec.Priority `json:"priority,omitzero"`
	// MemoryBytes and CPUs are the reservations; zero means
	// defaults.reservations.
	MemoryBytes int64   `json:"memory_bytes,omitempty"`
	CPUs        float64 `json:"cpus,omitempty"`
	// DependsOn are the services of the stack that this service needs.
	DependsOn []string `json:"depends_on,omitempty"`
}

// OwnerCLI owns the resources users apply with the CLI. A resource has one
// owner, so a later channel (GitOps) and the CLI never overwrite each other.
const OwnerCLI = "cli"

// StackStatus says what the stack is doing.
type StackStatus string

// The states of a stack.
const (
	StackPending  StackStatus = "pending"
	StackStarting StackStatus = "starting"
	StackRunning  StackStatus = "running"
	// StackFailing is a stack that its host could not bring up (UpError).
	// The host tries again at every pass.
	StackFailing StackStatus = "failing"
)

// Volume is a volume of a namespace and where its copies are.
type Volume struct {
	Meta
	Spec resource.VolumeSpec `json:"spec"`
	// DataGeneration grows with each new, empty LV (a volume of replicas 0
	// that moves, fails over or starts anew after a recover). It is part of
	// the LV's name (names.VolumeName) and at most names.MaxDataGeneration.
	DataGeneration int64        `json:"data_generation"`
	Minor          *int         `json:"minor,omitempty"` // DRBD minor; nil with replicas 0
	Copies         []VolumeCopy `json:"copies,omitempty"`
	Status         VolumeStatus `json:"status"`
	// Initialized says that a copy of this data generation was made
	// primary and got its file system (set-volume-initialized). Until then,
	// a host may promote with --force and format. After that, it never may:
	// a copy without a file system has then lost its data; it is not new.
	Initialized bool `json:"initialized,omitempty"`
}

// ID is the volume's key in State.Volumes.
func (v *Volume) ID() string {
	return names.ID(v.Namespace, v.Name)
}

// VolumeCopy is one copy of a volume, on one host.
type VolumeCopy struct {
	Host HostID `json:"host"`
	// Class is the class of the pool that the copy is in (any class may
	// fall back to another).
	Class      string `json:"class"`
	Tiebreaker bool   `json:"tiebreaker,omitempty"` // diskless
	// Synced is the host's report that the copy holds the volume's current
	// data (DRBD's UpToDate, or for a plain LV, once it exists). It is set
	// with set-copy-synced. A new copy is not synced until its first resync
	// is done. A tiebreaker holds no data and is never synced.
	Synced bool `json:"synced,omitempty"`
}

// RetiredCopy is a copy of a volume that its host may remove: a copy of a
// deleted stack or volume, a copy of an older generation, or a copy that
// the volume no longer places there. The host forgets it once the copy is
// gone (forget-copy).
type RetiredCopy struct {
	Volume         string `json:"volume"` // the volume's ID
	DataGeneration int64  `json:"data_generation"`
	Host           HostID `json:"host"`
}

// VolumeStatus says how healthy a volume's copies are.
type VolumeStatus string

// The states of a volume.
const (
	// VolumePending is a volume without a copy yet. A volume is placed
	// together with the first stack that names it.
	VolumePending VolumeStatus = "pending"
	VolumeOK      VolumeStatus = "ok"
	// VolumeRestorePending is a volume that lost every copy with its data
	// (clowderd recover). It gets no new copy until a restore brings its
	// data back.
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
