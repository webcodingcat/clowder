// Package state is the cluster's replicated state and the commands that
// change it. Every agent applies the same commands in Raft's order, so Apply
// must be deterministic: no I/O, no clock (a command carries its time), no
// randomness. docs/developing/state.md explains the rules.
package state

import (
	"maps"
	"net/netip"
	"slices"
	"time"

	"github.com/webcodingcat/clowder/internal/clusterspec"
	"github.com/webcodingcat/clowder/internal/hostrole"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/resource"
	"github.com/webcodingcat/clowder/internal/stackspec"
)

// MaxHosts is the most hosts a cluster can have. A host ID is also the
// DRBD node-id, the last part of the node address, and part of the
// WireGuard port (51800 + ID). stackspec.ReservedPort keeps 51800-51831
// free, so both constants change together.
const MaxHosts = 32

// HostID is a host's number, 0 to MaxHosts-1.
type HostID int

// Encrypted is a secret encrypted with age for the cluster's data key.
type Encrypted []byte

// State is everything the cluster agrees on. The zero value is an empty
// cluster that only accepts the bootstrap command.
type State struct {
	// Index is the Raft index of the last applied command. A read that must
	// see a certain write waits until Index reaches that write's index.
	Index       uint64        `json:"index"`
	Keys        *ClusterKeys  `json:"keys,omitempty"`
	Config      ClusterConfig `json:"config"`
	ClusterName string        `json:"cluster_name"`
	// JoinTokens are keyed by the token's ID.
	JoinTokens map[string]JoinToken `json:"join_tokens,omitempty"`
	Hosts      map[HostID]*Host     `json:"hosts,omitempty"`
	// HA is set for good once MinHAHosts control-plane hosts were in service.
	HA bool `json:"ha,omitempty"`
	// RecoveredAt is when recover-remap reduced the cluster to one host. It
	// becomes zero again once the cluster is HA.
	RecoveredAt time.Time `json:"recovered_at,omitzero"`
	// Forgetting maps each removed host whose ID is not free yet to the hosts
	// that still have to forget it as a DRBD peer.
	Forgetting map[HostID][]HostID `json:"forgetting,omitempty"`
	// Ingress is the host that should hold the Floating IP. The leader keeps
	// Hetzner's routing in line with it.
	Ingress *HostID `json:"ingress,omitempty"`
	// ACMEAccount is created by the leader with its first order.
	ACMEAccount *ACMEAccount `json:"acme_account,omitempty"`
	// Challenges are the HTTP-01 answers of the orders in progress, as
	// token -> key authorization. Every agent serves them.
	Challenges map[string]string `json:"challenges,omitempty"`
	// Certificates are the HTTP routes' certificates by domain.
	Certificates map[string]*Certificate `json:"certificates,omitempty"`
	// Revoked holds the revoked host certificates by serial (see revoke).
	Revoked map[string]Revocation `json:"revoked,omitempty"`
	// CLICutoff refuses every CLI certificate issued before it.
	CLICutoff  time.Time             `json:"cli_cutoff,omitzero"`
	Namespaces map[string]*Namespace `json:"namespaces,omitempty"`
	Stacks     map[string]*Stack     `json:"stacks,omitempty"`  // names.ID: "<namespace>/<stack>"
	Secrets    map[string]*Secret    `json:"secrets,omitempty"` // names.ID: "<namespace>/<secret>"
	Volumes    map[string]*Volume    `json:"volumes,omitempty"` // names.ID: "<namespace>/<volume>"
	// Retired are the only copies an agent removes, so a copy that is merely
	// missing from the state, because of a bug, keeps its data.
	Retired []RetiredCopy `json:"retired,omitempty"`
	// RetiredMinors maps the DRBD minor of each volume that still has a
	// retired copy to the volume's ID. retire takes a minor, and
	// forgetRetiredMinors gives it back.
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

// ClusterKeys are the keys that init creates. The CA certificate is public;
// the CA's key is encrypted with the data key.
type ClusterKeys struct {
	CACertificate []byte    `json:"ca_certificate"`
	CAKey         Encrypted `json:"ca_key"`
	// SealingKey is the age identity that SealedSecrets are sealed for.
	SealingKey Encrypted `json:"sealing_key"`
}

// ClusterConfig is the applied ClusterConfiguration, with the defaults
// filled in. Until the first apply, its generation is 0 and its spec empty.
type ClusterConfig struct {
	Meta
	Spec clusterspec.Config `json:"spec"`
}

// HostPhase is the stage of a host's life. phaseChanges lists the allowed
// changes between phases.
type HostPhase string

// The phases of a host. Failed, fencing, fenced, recovering and disabled
// belong to the failover (fencing.go in the controller).
const (
	Joining    HostPhase = "joining"
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
var Phases = []HostPhase{Joining, Active, Draining, Failed, Fencing, Fenced, Recovering, Disabled, Removed}

// Host is one member of the cluster.
type Host struct {
	ID   HostID `json:"id"`
	Name string `json:"name"`
	// Roles are fixed when the host joined; its certificate names the same
	// roles.
	Roles    hostrole.Set `json:"roles"`
	ServerID int64        `json:"server_id"`
	// ServerChecked says that Hetzner confirmed the server has the host's
	// addresses. A host names its server itself, so only a checked host may
	// get the Floating IP or be fenced.
	ServerChecked bool       `json:"server_checked,omitempty"`
	Underlay      netip.Addr `json:"underlay"`
	// PublicAddress is the server's public IPv4, where users reach the
	// host's API.
	PublicAddress       netip.Addr `json:"public_address"`
	WGPublicKey         string     `json:"wg_public_key"`
	CertificateSerial   string     `json:"certificate_serial"`
	CertificateNotAfter time.Time  `json:"certificate_not_after"`
	Voter               bool       `json:"voter"`
	Phase               HostPhase  `json:"phase"`
	Capacity            Resources  `json:"capacity"`
	// Pools are the host's thin pools by name ("vg/thin"). Storage classes
	// name pools.
	Pools      map[string]Pool `json:"pools,omitempty"`
	Version    string          `json:"version,omitempty"`
	DockerDown bool            `json:"docker_down,omitempty"`
	// RootUsedPercent is in whole percent.
	RootUsedPercent int `json:"root_used_percent,omitempty"`
	// SplitBrain lists the volumes, by ID, whose copy on this host is cut
	// off from a peer by a split brain that DRBD could not resolve.
	// NoQuorum lists the volumes whose primary copy on this host lacks
	// DRBD's quorum, so the writes of their stack hang. Both are sorted.
	SplitBrain []string `json:"split_brain,omitempty"`
	NoQuorum   []string `json:"no_quorum,omitempty"`
	// Leaving marks a host that the leader removes once nothing is left on
	// it (SetHostPhase.Leaving).
	Leaving bool `json:"leaving,omitempty"`
	// FencedAt holds the times the host was fenced within FlapWindow,
	// oldest first (Flapping). Enabling the host clears them.
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
	// Owner is the channel that applied the resource; only it changes or
	// deletes the resource. names.Default and names.System have none.
	Owner      string `json:"owner,omitempty"`
	Generation int64  `json:"generation"`
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
	Spec EncryptedSecret `json:"spec"`
}

// EncryptedSecret is a secret's spec as the state stores it, with its values
// encrypted with the data key. The same value encrypts differently every
// time, so the digest, not the values, tells a change apart.
type EncryptedSecret struct {
	Data map[string]Encrypted `json:"data"`
	// Digest is an HMAC over the secret's ID, keys and values, with a key
	// derived from the data key.
	Digest string `json:"digest"`
	// KeyDigests holds one HMAC per key, over the value, the key and the
	// secret's ID, so a diff can tell which value changed.
	KeyDigests map[string]string `json:"key_digests"`
}

// Keys are the secret's keys, sorted.
func (s EncryptedSecret) Keys() []string {
	return slices.Sorted(maps.Keys(s.Data))
}

// Stack is a stack of a namespace: its normalized spec and its status, which
// the leader and the stack's host write.
type Stack struct {
	Meta
	Spec stackspec.Spec `json:"spec"`
	// Services holds what the scheduler needs of each service, by name,
	// worked out when the stack is put (servicesOf), so the scheduler does
	// not read the spec again.
	Services map[string]Service `json:"services,omitempty"`
	Host     *HostID            `json:"host,omitempty"` // nil: not placed yet
	Status   StackStatus        `json:"status"`
	// UpError is the shortened first line of why the host could not bring
	// the stack up, while it is failing. It is only for display.
	UpError string `json:"up_error,omitempty"`
	// AppliedSecrets holds the digests of the secrets, by name, that the
	// containers on the stack's host came up with.
	AppliedSecrets map[string]string `json:"applied_secrets,omitempty"`
	// PushedOut are the services stopped for services of higher priority,
	// sorted. Only the leader changes the list.
	PushedOut []string `json:"pushed_out,omitempty"`
	// Restarted is when a user last restarted each service, by name. The
	// stack's host recreates a service whose time changed.
	Restarted map[string]time.Time `json:"restarted,omitempty"`
	// WaitReason is only for display; it is empty while nothing waits.
	WaitReason string `json:"wait_reason,omitempty"`
	// WaitingSince stays when only the reason's text changes. It is in the
	// state so that it survives a change of leader; the alert for a stack
	// that waits too long reads it.
	WaitingSince time.Time `json:"waiting_since,omitzero"`
	// MoveTo is the host a user asked the stack to move to. The end of the
	// move clears it, whether the move worked or not.
	MoveTo *HostID `json:"move_to,omitempty"`
	// LastMove and MoveFailed start the placement's cooldown (MoveCooldown).
	LastMove   time.Time `json:"last_move,omitzero"`
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
	MemoryBytes int64    `json:"memory_bytes,omitempty"`
	CPUs        float64  `json:"cpus,omitempty"`
	DependsOn   []string `json:"depends_on,omitempty"`
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
	// DataGeneration is part of the LV's name (names.VolumeName) and at most
	// names.MaxDataGeneration.
	DataGeneration int64        `json:"data_generation"`
	Minor          *int         `json:"minor,omitempty"` // DRBD minor; nil with replicas 0
	Copies         []VolumeCopy `json:"copies,omitempty"`
	Status         VolumeStatus `json:"status"`
	// Initialized says that a copy of this data generation got its file
	// system. From then on, no host may promote with --force and format.
	Initialized bool `json:"initialized,omitempty"`
}

// ID is the volume's key in State.Volumes.
func (v *Volume) ID() string {
	return names.ID(v.Namespace, v.Name)
}

// VolumeCopy is one copy of a volume, on one host.
type VolumeCopy struct {
	Host HostID `json:"host"`
	// Class is the class of the pool that holds the copy. It may differ
	// from the volume's class, because any class may fall back to another.
	Class      string `json:"class"`
	Tiebreaker bool   `json:"tiebreaker,omitempty"` // diskless
	// Synced is the host's report that the copy holds the volume's current
	// data (DRBD's UpToDate, or for a plain LV, once it exists). A tiebreaker
	// holds no data and is never synced.
	Synced bool `json:"synced,omitempty"`
}

// RetiredCopy is a volume copy that its host may remove. Once the copy is
// gone, the host forgets it with forget-copy.
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

const auditSize = 10000
