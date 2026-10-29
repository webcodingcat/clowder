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
	"github.com/webcodingcat/clowder/internal/clusterdir"
)

// MaxHosts is the size of a cluster: a host ID is also the DRBD node-id and
// the last part of the node address and the WireGuard port (51800 + ID;
// compose.ReservedPort keeps 51800-51831 free, so both change together).
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
	Index      uint64                `json:"index"`
	Keys       *ClusterKeys          `json:"keys,omitempty"`
	Config     ClusterConfig         `json:"config"`
	Secrets    map[string]Sealed     `json:"secrets,omitempty"`
	JoinKeys   []Sealed              `json:"join_keys,omitempty"`
	Registries map[string]Sealed     `json:"registries,omitempty"`
	Hosts      map[HostID]*Host      `json:"hosts,omitempty"`
	Users      map[string]*User      `json:"users,omitempty"`
	Revoked    map[string]time.Time  `json:"revoked,omitempty"` // certificate serial -> when
	Stacks     map[string]*Stack     `json:"stacks,omitempty"`
	Volumes    map[string]*Volume    `json:"volumes,omitempty"` // "<stack>/<volume>"
	Operations map[string]*Operation `json:"operations,omitempty"`
	Locks      map[string]string     `json:"locks,omitempty"` // "stack/<name>" or "volume/<id>" -> operation
	Audit      []AuditEntry          `json:"audit,omitempty"`
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
	Hash string            `json:"hash,omitempty"`
	Spec clusterdir.Config `json:"spec"`
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

// Host is one member of the cluster.
type Host struct {
	ID          HostID     `json:"id"`
	Name        string     `json:"name"`
	ServerRef   string     `json:"server_ref,omitempty"` // the hoster's ID of the server
	Underlay    netip.Addr `json:"underlay"`
	WGPublicKey string     `json:"wg_public_key"`
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
}

// Resources are what a host offers after the system reserve.
type Resources struct {
	MemoryBytes int64   `json:"memory_bytes"`
	CPUs        float64 `json:"cpus"`
}

// Pool is a thin pool's size and how much of it volumes have taken.
type Pool struct {
	Name      string `json:"name"` // volume-group/thin-pool
	SizeBytes int64  `json:"size_bytes"`
	UsedBytes int64  `json:"used_bytes"`
}

// User may use the CLI with the certificates listed here.
type User struct {
	Name string `json:"name"`
	// Serials of the user's certificates; removing the user revokes them.
	Serials []string `json:"serials,omitempty"`
}

// Stack is a registered Compose stack.
type Stack struct {
	Name  string `json:"name"`
	Owner string `json:"owner"` // the channel: OwnerCLI or GitOpsOwner(source)
	// CreatedBy is the actor that registered the stack: a user's name, or
	// the actor itself for anyone else.
	CreatedBy string `json:"created_by,omitempty"`
	// Generation counts the applies of this stack.
	Generation int64 `json:"generation"`
	// Source is the stack's Compose file as the CLI checked and sent it
	// (compose.Stack.Source): self-contained, x-clowder included.
	Source   string      `json:"source"`
	Priority int         `json:"priority"`
	Host     *HostID     `json:"host,omitempty"` // nil: not placed yet
	Status   StackStatus `json:"status"`
	// Stopped lists services pushed out by higher priorities.
	Stopped  []string  `json:"stopped,omitempty"`
	LastMove time.Time `json:"last_move,omitzero"`
}

// OwnerCLI owns the stacks applied with clowder stack apply.
const OwnerCLI = "cli"

// GitOpsOwner owns the stacks of a GitOps source; a stack has one owner, so
// the CLI and a source never overwrite each other.
func GitOpsOwner(source string) string {
	return "gitops:" + source
}

// StackStatus says what the stack is doing.
type StackStatus string

// The states of a stack.
const (
	// StackCreated is registered and never applied: it has no Source, and
	// nothing places or runs it until put-stack moves it to pending.
	StackCreated        StackStatus = "created"
	StackPending        StackStatus = "pending"
	StackStarting       StackStatus = "starting"
	StackRunning        StackStatus = "running"
	StackWaitingForHost StackStatus = "waiting-for-host"
	StackStopped        StackStatus = "stopped"
)

// Volume is a named volume of a stack.
type Volume struct {
	ID string `json:"id"` // "<stack>/<volume>"
	// Generation grows with a restore or a class move; it is part of the
	// Docker volume's name.
	Generation int64        `json:"generation"`
	Class      string       `json:"class"`
	SizeBytes  int64        `json:"size_bytes"`
	Replicas   int          `json:"replicas"`
	Minor      *int         `json:"minor,omitempty"` // DRBD minor; nil with replicas 0
	Placements []Placement  `json:"placements,omitempty"`
	Status     VolumeStatus `json:"status"`
}

// Placement is one copy of a volume.
type Placement struct {
	Host HostID `json:"host"`
	// Class is the pool's class the copy is in (any falls back to another).
	Class      string `json:"class"`
	Tiebreaker bool   `json:"tiebreaker,omitempty"` // diskless
}

// VolumeStatus says how healthy a volume's copies are.
type VolumeStatus string

// The states of a volume.
const (
	VolumeOK             VolumeStatus = "ok"
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
