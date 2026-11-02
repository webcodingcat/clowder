package state

import (
	"errors"
	"fmt"
	"slices"

	"github.com/webcodingcat/clowder/internal/clusterspec"
	"github.com/webcodingcat/clowder/internal/names"
)

// Bootstrap starts the cluster: the keys init made, its bootstrap key and
// the first host.
type Bootstrap struct {
	Keys    ClusterKeys  `json:"keys"`
	JoinKey SealedSecret `json:"join_key"`
	Host    Host         `json:"host"`
}

// FirstJoinKey is the name of init's bootstrap key: the first entry of
// cluster.yaml's join_keys, which the bootstrap's cluster.yaml writes.
const FirstJoinKey = "join_keys[0]"

// applyBootstrap makes the first host the cluster. Until the first
// cluster.yaml is applied, the config is revision 0 and names only the
// bootstrap key, so the user and the next hosts get in with it.
func applyBootstrap(s *State, _ Command, payload Bootstrap) (func(), error) {
	switch {
	case s.Keys != nil:
		return nil, errors.New("the cluster is bootstrapped already")
	case len(payload.Keys.CACertificate) == 0 || len(payload.Keys.CAKey) == 0 || len(payload.Keys.AgeIdentity) == 0:
		return nil, errors.New("the keys are incomplete")
	case len(payload.JoinKey.Value) == 0 || payload.JoinKey.Digest == "":
		return nil, errors.New("the bootstrap key is missing")
	case payload.Host.ID != 0:
		return nil, fmt.Errorf("the first host has ID 0, not %d", payload.Host.ID)
	}
	if err := checkNewHost(payload.Host); err != nil {
		return nil, err
	}
	host := payload.Host
	host.Voter, host.Phase = true, Active
	keys := payload.Keys
	return func() {
		s.Keys = &keys
		s.Config = ClusterConfig{Spec: clusterspec.Config{JoinKeys: []clusterspec.SecretRef{FirstJoinKey}}}
		s.ClusterSecrets = map[string]SealedSecret{FirstJoinKey: payload.JoinKey}
		s.Hosts[host.ID] = &host
		s.Namespaces[names.Default] = &Namespace{Meta: Meta{Name: names.Default, Generation: 1}}
	}, nil
}

// checkNewHost checks what a host must bring.
func checkNewHost(host Host) error {
	switch {
	case host.Name == "":
		return errors.New("the host has no name")
	case !host.Underlay.IsValid():
		return fmt.Errorf("host %s has no underlay address", host.Name)
	case host.WGPublicKey == "":
		return fmt.Errorf("host %s has no WireGuard key", host.Name)
	case host.CertificateSerial == "":
		return fmt.Errorf("host %s has no certificate serial", host.Name)
	case host.CertificateNotAfter.IsZero():
		return fmt.Errorf("host %s: the certificate has no expiry", host.Name)
	}
	return nil
}

// SetHostPhase moves a host to another phase.
type SetHostPhase struct {
	Host  HostID    `json:"host"`
	Phase HostPhase `json:"phase"`
	Voter *bool     `json:"voter,omitempty"` // changes the Raft role along with the phase
	// From, when set, is the phase the sender checked the move against; the
	// move is refused if the host has left it since.
	From HostPhase `json:"from,omitempty"`
}

// mover says who may make a phase change: the leader (on its own or for a
// user), or the host itself when it reports what only it can see. A host
// never moves another host. The API checks which actor may send which kind
// of command at all. The state checks only what needs the state: whether
// the reporting host is the host the report is about.
type mover int

const (
	byLeaderOrUser mover = iota
	byTheHost
)

// phaseChanges are the allowed moves; any phase may also go to Removed, by
// the leader or a user, unless the host is the last that runs stacks.
// The table is the lifecycle of phase 2: nothing reaches joining or passive
// before the join, nor failed, fenced, recovering or disabled before failover.
var phaseChanges = map[HostPhase]map[HostPhase]mover{
	Joining:    {Passive: byLeaderOrUser, Active: byLeaderOrUser},
	Passive:    {Active: byLeaderOrUser},
	Active:     {Draining: byLeaderOrUser, Failed: byLeaderOrUser},
	Draining:   {Active: byLeaderOrUser, Failed: byLeaderOrUser},
	Failed:     {Active: byTheHost, Fenced: byLeaderOrUser},
	Fenced:     {Recovering: byTheHost, Disabled: byLeaderOrUser},
	Recovering: {Active: byTheHost},
	Disabled:   {Fenced: byLeaderOrUser},
}

// runsStacks tells whether a host in this phase runs stacks: a draining
// host keeps those it has.
func runsStacks(phase HostPhase) bool {
	return phase == Active || phase == Draining
}

// checkRemoval refuses to remove the last host that runs stacks: the
// cluster would go on without placement, ingress or alerts, and nothing
// brings a removed host back. A host that runs none (failed, fenced) may
// go, so a dead host can always be given up.
func (s *State) checkRemoval(host *Host) error {
	if !runsStacks(host.Phase) {
		return nil
	}
	for _, other := range s.Hosts {
		if other.ID != host.ID && runsStacks(other.Phase) {
			return nil
		}
	}
	return fmt.Errorf("host %s is the last that runs stacks; the cluster cannot do without it", host.Name)
}

func applySetHostPhase(s *State, cmd Command, payload SetHostPhase) (func(), error) {
	host, ok := s.Hosts[payload.Host]
	if !ok {
		return nil, fmt.Errorf("no host %d", payload.Host)
	}
	if host.Phase == Removed {
		return nil, fmt.Errorf("host %s is removed", host.Name)
	}
	if payload.From != "" && payload.From != host.Phase {
		return nil, fmt.Errorf("host %s is %s now, no longer %s; try again", host.Name, host.Phase, payload.From)
	}
	who, allowed := phaseChanges[host.Phase][payload.Phase]
	if payload.Phase == Removed {
		if err := s.checkRemoval(host); err != nil {
			return nil, err
		}
		who, allowed = byLeaderOrUser, true
	}
	if !allowed {
		return nil, fmt.Errorf("host %s cannot go from %s to %s", host.Name, host.Phase, payload.Phase)
	}
	fromHost := IsHostActor(cmd.Actor)
	switch {
	case who == byTheHost && cmd.Actor != HostActor(host.ID):
		return nil, fmt.Errorf("only host %s itself reports %s", host.Name, payload.Phase)
	case who == byLeaderOrUser && fromHost:
		return nil, fmt.Errorf("%s cannot move host %s to %s; only the leader or a user does", cmd.Actor, host.Name, payload.Phase)
	}
	return func() {
		host.Phase = payload.Phase
		if payload.Voter != nil {
			host.Voter = *payload.Voter
		}
		if host.Phase == Removed {
			host.Voter = false
			s.revoke(host.CertificateSerial, host.CertificateNotAfter, cmd.At)
			// nobody is left to remove the host's retired copies
			s.Retired = slices.DeleteFunc(s.Retired, func(c RetiredCopy) bool { return c.Host == host.ID })
		}
	}, nil
}

// SetIngress records the host that should hold the Floating IP. Only an
// active host with a ServerRef can: the hoster routes the address to a
// server by that ID.
type SetIngress struct {
	Host HostID `json:"host"`
}

func applySetIngress(s *State, _ Command, payload SetIngress) (func(), error) {
	host, ok := s.Hosts[payload.Host]
	switch {
	case !ok:
		return nil, fmt.Errorf("no host %d", payload.Host)
	case host.Phase != Active:
		return nil, fmt.Errorf("host %s is %s, not active", host.Name, host.Phase)
	case host.ServerRef == "":
		return nil, fmt.Errorf("host %s has no server ref", host.Name)
	}
	id := payload.Host
	return func() { s.Ingress = &id }, nil
}

// ReportHost is an agent telling what it is and how it is doing: the
// version it runs, its memory and CPUs, its thin pools (by pool name, "vg/thin"),
// whether Docker answers and how full the root file system is. The leader
// places by it and raises alerts from it. Agents send it when something
// changed, not on every look, so every value is one that changes seldom: a
// bool, a whole percent.
type ReportHost struct {
	Host            HostID          `json:"host"`
	Version         string          `json:"version"`
	Capacity        Resources       `json:"capacity"`
	Pools           map[string]Pool `json:"pools,omitempty"`
	DockerDown      bool            `json:"docker_down,omitempty"`
	RootUsedPercent int             `json:"root_used_percent,omitempty"`
}

func applyReportHost(s *State, cmd Command, payload ReportHost) (func(), error) {
	host, ok := s.Hosts[payload.Host]
	switch {
	case !ok:
		return nil, fmt.Errorf("no host %d", payload.Host)
	case cmd.Actor != HostActor(host.ID):
		return nil, fmt.Errorf("only host %s itself reports about itself", host.Name)
	case payload.Version == "":
		return nil, errors.New("the version is empty")
	case !percent(payload.RootUsedPercent):
		return nil, fmt.Errorf("host %s: the root file system is %d %% full", host.Name, payload.RootUsedPercent)
	}
	for name, pool := range payload.Pools {
		if pool.SizeBytes <= 0 || !percent(pool.DataPercent) || !percent(pool.MetadataPercent) {
			return nil, fmt.Errorf("host %s: pool %s with size %d, data %d %% and metadata %d %% full",
				host.Name, name, pool.SizeBytes, pool.DataPercent, pool.MetadataPercent)
		}
	}
	return func() {
		host.Version, host.Capacity, host.Pools = payload.Version, payload.Capacity, payload.Pools
		host.DockerDown, host.RootUsedPercent = payload.DockerDown, payload.RootUsedPercent
	}, nil
}

// percent tells whether a value is a whole percent, 0 to 100.
func percent(value int) bool {
	return value >= 0 && value <= 100
}
