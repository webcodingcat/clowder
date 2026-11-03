package state

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/resource"
)

// Bootstrap starts the cluster: the keys init made, its bootstrap key
// (sealed) and the first host.
type Bootstrap struct {
	Keys         ClusterKeys `json:"keys"`
	BootstrapKey Sealed      `json:"bootstrap_key"`
	Host         Host        `json:"host"`
}

// applyBootstrap makes the first host the cluster. The bootstrap key lets
// the user in, once (applyAddUser drops it); hosts join with tokens.
func applyBootstrap(s *State, _ Command, payload Bootstrap) (func(), error) {
	switch {
	case s.Keys != nil:
		return nil, errors.New("the cluster is bootstrapped already")
	case len(payload.Keys.CACertificate) == 0 || len(payload.Keys.CAKey) == 0 || len(payload.Keys.AgeIdentity) == 0:
		return nil, errors.New("the keys are incomplete")
	case len(payload.BootstrapKey) == 0:
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
		s.BootstrapKey = payload.BootstrapKey
		s.Hosts[host.ID] = &host
		s.Namespaces[names.Default] = &Namespace{Meta: Meta{Name: names.Default, Generation: 1}}
	}, nil
}

// checkNewHost checks what a host must bring.
func checkNewHost(host Host) error {
	switch {
	case host.Name == "":
		return errors.New("the host has no name")
	// it names the host in DRBD's resource files, and is its hostname
	case !names.Valid(host.Name):
		return fmt.Errorf("host name %q: %s", host.Name, names.Rule)
	// the nft rules and WireGuard's endpoints are IPv4
	case !host.Underlay.Is4():
		return fmt.Errorf("host %s has no IPv4 underlay address", host.Name)
	case host.PublicAddress.IsValid() && !host.PublicAddress.Is4():
		return fmt.Errorf("host %s: the public address %s is not IPv4", host.Name, host.PublicAddress)
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
	// From, when set, is the phase the sender checked the move against; the
	// move is refused if the host has left it since.
	From HostPhase `json:"from,omitempty"`
	// Operation is the leader's operation that makes the move, if any:
	// while one holds the host's lock (fencing, drain, remove), only it
	// moves the host, and the host itself reports no phase either.
	Operation uint64 `json:"operation,omitempty"`
	// Leaving goes with draining only: a user removes a host that still
	// has stacks or volume copies (Host.Leaving). Any other phase change
	// takes it back.
	Leaving bool `json:"leaving,omitempty"`
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
// the leader or a user, unless checkRemoval refuses. A host joins as
// joining (add-host); nothing reaches failed, fenced, recovering or
// disabled before failover.
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

// MinHAHosts is how many hosts run stacks in a cluster that is highly
// available: three voters, a majority of which survives one host.
const MinHAHosts = 3

// hostsRunningStacks counts the hosts that run stacks. The hosts in
// leftOut are not counted.
func (s *State) hostsRunningStacks(leftOut ...HostID) int {
	count := 0
	for _, other := range s.Hosts {
		if !slices.Contains(leftOut, other.ID) && runsStacks(other.Phase) {
			count++
		}
	}
	return count
}

// CheckRemoval tells whether the state would remove the host (checkRemoval);
// the API asks it before the host hands its lead over.
func (s *State) CheckRemoval(id HostID) error {
	host, ok := s.Hosts[id]
	switch {
	case !ok:
		return fmt.Errorf("no host %d", id)
	case host.Phase == Removed:
		return fmt.Errorf("host %s is removed", host.Name)
	}
	return s.checkRemoval(host)
}

// CheckLeaving tells whether a host may leave (checkMayGo): the API asks it
// when CheckRemoval refuses a host for its stacks and copies, which then
// move off it first (SetHostPhase.Leaving).
func (s *State) CheckLeaving(id HostID) error {
	host, ok := s.Hosts[id]
	switch {
	case !ok:
		return fmt.Errorf("no host %d", id)
	case host.Phase != Active && host.Phase != Draining:
		return fmt.Errorf("host %s is %s; only an active or draining host leaves", host.Name, host.Phase)
	}
	return s.checkMayGo(host)
}

// checkRemoval refuses to remove a host the cluster cannot do without
// (checkMayGo), or a host that runs stacks while a stack or a volume copy
// is still on it: they would be lost with it. Such a host leaves instead
// (SetHostPhase.Leaving): they move off it, and then it goes.
func (s *State) checkRemoval(host *Host) error {
	if err := s.checkMayGo(host); err != nil || !runsStacks(host.Phase) {
		return err
	}
	return s.checkEmpty(host)
}

// checkMayGo refuses to let a host go that the cluster cannot do without:
//   - the last host that runs stacks: the cluster would go on without
//     placement, ingress or alerts, and nothing brings a removed host back;
//   - once the cluster is highly available, any host that would leave fewer
//     than MinHAHosts that run stacks, a dead one too: a replacement joins
//     first. A cluster never goes back below three; the one way out is
//     clowderd recover --power-off, which phase 2 brings for a quorum that
//     is lost for good.
//
// Before the cluster is highly available, a host that runs no stacks
// (failed, fenced) may always go, so a dead host can be given up.
func (s *State) checkMayGo(host *Host) error {
	// a host that leaves goes too, so it does not count as one that stays
	going := []HostID{host.ID}
	for _, other := range s.Hosts {
		if other.Leaving {
			going = append(going, other.ID)
		}
	}
	remaining := s.hostsRunningStacks(going...)
	switch {
	case s.HA && remaining < MinHAHosts:
		return fmt.Errorf("host %s cannot go: %d hosts would be left to run stacks, and a highly available "+
			"cluster keeps %d; join a replacement first", host.Name, remaining, MinHAHosts)
	case runsStacks(host.Phase) && remaining == 0:
		return fmt.Errorf("host %s is the last that runs stacks; the cluster cannot do without it", host.Name)
	}
	return nil
}

// ErrNotEmpty is checkRemoval's answer for a host that still has a stack
// or a volume copy.
var ErrNotEmpty = errors.New("it must move off first")

// checkEmpty refuses a host that still has a stack or a volume copy
// (ErrNotEmpty).
func (s *State) checkEmpty(host *Host) error {
	for _, id := range slices.Sorted(maps.Keys(s.Stacks)) {
		if placed := s.Stacks[id].Host; placed != nil && *placed == host.ID {
			return fmt.Errorf("host %s still has stack %s: %w", host.Name, id, ErrNotEmpty)
		}
	}
	for _, id := range slices.Sorted(maps.Keys(s.Volumes)) {
		if slices.ContainsFunc(s.Volumes[id].Copies, func(c VolumeCopy) bool { return c.Host == host.ID }) {
			return fmt.Errorf("host %s still has a copy of volume %s: %w", host.Name, id, ErrNotEmpty)
		}
	}
	return nil
}

// remove makes the host removed: its certificate revoked, its retired
// copies forgotten, and every host that is left to forget it as a DRBD
// peer (forgetPeer). Its ID is free once all of them have.
func (s *State) remove(host *Host, at time.Time) {
	host.Phase, host.Voter = Removed, false
	s.revoke(host.CertificateSerial, host.CertificateNotAfter, at)
	// nobody is left to remove the host's retired copies
	s.Retired = slices.DeleteFunc(s.Retired, func(c RetiredCopy) bool { return c.Host == host.ID })
	// the removed host forgets nothing any more
	for peer := range s.Forgetting {
		s.forgot(host.ID, peer)
	}
	var waiting []HostID
	for _, id := range slices.Sorted(maps.Keys(s.Hosts)) {
		if id != host.ID && s.Hosts[id].Phase != Removed {
			waiting = append(waiting, id)
		}
	}
	if len(waiting) > 0 {
		s.Forgetting[host.ID] = waiting
	}
}

// forgot takes a host off the list of those that have still to forget the
// peer, and the peer off Forgetting once none is left.
func (s *State) forgot(host, peer HostID) {
	waiting := slices.DeleteFunc(s.Forgetting[peer], func(id HostID) bool { return id == host })
	if len(waiting) == 0 {
		delete(s.Forgetting, peer)
	} else {
		s.Forgetting[peer] = waiting
	}
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
	if payload.Operation != 0 && cmd.Actor != LeaderActor {
		return nil, fmt.Errorf("%s cannot act for operation %d; only the leader runs operations", cmd.Actor, payload.Operation)
	}
	if err := s.checkUnlocked(hostLock(host.ID), payload.Operation); err != nil {
		return nil, err
	}
	who, allowed := phaseChanges[host.Phase][payload.Phase]
	switch {
	case payload.Leaving && payload.Phase != Draining:
		return nil, fmt.Errorf("host %s: only a draining host leaves", host.Name)
	case payload.Leaving:
		// a remove of a host that still has stacks or copies: it may go
		// as soon as they moved
		if err := s.checkMayGo(host); err != nil {
			return nil, err
		}
		allowed = allowed || host.Phase == Draining
	case payload.Phase == Removed:
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
		if payload.Phase == Removed {
			s.remove(host, cmd.At)
			return
		}
		host.Phase = payload.Phase
		// an undrain takes the remove back
		host.Leaving = payload.Leaving
		if payload.Phase != Active || payload.Leaving {
			s.forgetMovesTo(host.ID)
		}
		if s.hostsRunningStacks() >= MinHAHosts {
			s.HA = true
		}
	}, nil
}

// SetHostVoter records whether a host votes in Raft, after the leader
// changed Raft's configuration (controller's members loop): the state
// mirrors Raft, which decides.
type SetHostVoter struct {
	Host  HostID `json:"host"`
	Voter bool   `json:"voter"`
}

func applySetHostVoter(s *State, cmd Command, payload SetHostVoter) (func(), error) {
	host, ok := s.Hosts[payload.Host]
	switch {
	case cmd.Actor != LeaderActor:
		return nil, fmt.Errorf("%s cannot record a voter; only the leader does", cmd.Actor)
	case !ok:
		return nil, fmt.Errorf("no host %d", payload.Host)
	case host.Phase == Removed:
		return nil, fmt.Errorf("host %s is removed", host.Name)
	case host.Voter == payload.Voter:
		return nil, errUnchanged
	}
	return func() { host.Voter = payload.Voter }, nil
}

// ReportPeerForgotten is a host telling that it forgot a removed host as a
// DRBD peer (drbdsetup forget-peer on each of its resources), so a new
// host may take the ID.
type ReportPeerForgotten struct {
	Host HostID `json:"host"`
	Peer HostID `json:"peer"`
}

func applyReportPeerForgotten(s *State, cmd Command, payload ReportPeerForgotten) (func(), error) {
	waiting, ok := s.Forgetting[payload.Peer]
	switch {
	case cmd.Actor != HostActor(payload.Host):
		return nil, fmt.Errorf("only host %d itself reports what it forgot", payload.Host)
	case !ok || !slices.Contains(waiting, payload.Host):
		return nil, errUnchanged
	}
	return func() { s.forgot(payload.Host, payload.Peer) }, nil
}

// SetIngress records the host that should hold the Floating IP. Only an
// active host with a checked ServerRef can: the hoster routes the address
// to a server by that ID.
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
	case !host.ServerRefChecked:
		return nil, fmt.Errorf("host %s has no checked server ref", host.Name)
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

// forgetMovesTo drops the users' requests to move stacks to a host that no
// longer takes stacks: they would wait for it for good.
func (s *State) forgetMovesTo(id HostID) {
	for _, stack := range s.Stacks {
		if stack.MoveTo != nil && *stack.MoveTo == id {
			stack.MoveTo = nil
			s.changed(resource.KindStack, &stack.Meta)
		}
	}
}
