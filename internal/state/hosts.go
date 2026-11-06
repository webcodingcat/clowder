package state

import (
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"time"

	"github.com/webcodingcat/clowder/internal/clusterspec"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/resource"
)

// Bootstrap starts the cluster (clowderd init) with the keys that init
// made, the cluster's name and the first host.
type Bootstrap struct {
	Keys        ClusterKeys `json:"keys"`
	ClusterName string      `json:"cluster_name"`
	Host        Host        `json:"host"`
}

// applyBootstrap makes the first host the cluster. The CLI's first
// credentials come from root on that host (clowderd credentials); other
// hosts join with tokens.
func applyBootstrap(s *State, _ Command, payload Bootstrap) (func(), error) {
	switch {
	case s.Keys != nil:
		return nil, errors.New("the cluster is bootstrapped already")
	case len(payload.Keys.CACertificate) == 0 || len(payload.Keys.CAKey) == 0 || len(payload.Keys.AgeIdentity) == 0:
		return nil, errors.New("the keys are incomplete")
	case !names.Valid(payload.ClusterName):
		return nil, fmt.Errorf("cluster name %q: %s", payload.ClusterName, names.Rule)
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
		s.ClusterName = payload.ClusterName
		s.Hosts[host.ID] = &host
		s.Namespaces[names.Default] = &Namespace{Meta: Meta{Name: names.Default, Generation: 1}}
		s.Namespaces[names.System] = &Namespace{Meta: Meta{Name: names.System, Generation: 1}}
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
	case !host.PublicAddress.Is4():
		return fmt.Errorf("host %s has no IPv4 public address", host.Name)
	// the Floating IP and fencing act on the server
	case host.ServerID <= 0:
		return fmt.Errorf("host %s has no server ID", host.Name)
	case host.WGPublicKey == "":
		return fmt.Errorf("host %s has no WireGuard key", host.Name)
	case host.CertificateSerial == "":
		return fmt.Errorf("host %s has no certificate serial", host.Name)
	case host.CertificateNotAfter.IsZero():
		return fmt.Errorf("host %s: the certificate has no expiry", host.Name)
	}
	return nil
}

// checkUnderlay makes sure the host's underlay is in the configuration's
// private network: the hosts reach each other there, and the containers'
// boundaries close it.
func checkUnderlay(config clusterspec.Config, host *Host) error {
	network, err := netip.ParsePrefix(config.PrivateNetwork)
	if err != nil {
		return nil // no configuration yet: Check lets no other value in
	}
	if !network.Contains(host.Underlay) {
		return fmt.Errorf("host %s has the underlay %s, outside privateNetwork %s", host.Name, host.Underlay, network)
	}
	return nil
}

// SetHostPhase moves a host to another phase.
type SetHostPhase struct {
	Host  HostID    `json:"host"`
	Phase HostPhase `json:"phase"`
	// From, when set, is the phase that the sender checked the move
	// against. The move is refused if the host has left that phase since.
	From HostPhase `json:"from,omitempty"`
	// Operation is the leader's operation that makes the move, if any.
	// While an operation holds the host's lock (fencing, drain, remove),
	// only that operation moves the host, and the host itself reports no
	// phase either.
	Operation uint64 `json:"operation,omitempty"`
	// Leaving is set only with draining: a user removes a host that still
	// has stacks or volume copies (Host.Leaving). An undrain takes it
	// back; a failure keeps it, and the host leaves once it is back.
	Leaving bool `json:"leaving,omitempty"`
}

// mover says who may make a phase change: the leader (on its own or for the
// CLI), or the host itself when it reports what only it can see. A host
// never moves another host. The API checks which actor may send which kind
// of command at all. The state checks only what needs the state: whether
// the reporting host is the host the report is about.
type mover int

const (
	byLeaderOrCLI mover = iota
	byTheHost
)

// phaseChanges are the allowed moves. The leader or the CLI may also move a
// host in any phase to Removed, unless checkRemoval refuses. A host joins
// as joining (add-host). It becomes fencing only when a fence-host
// operation starts (StartOperation.Fence), which holds the host's lock.
// Only that operation moves the host on: to fenced once its server is off,
// or back to failed when it could not power the server off.
//
// A failed host that is heard again becomes active once more, but only the
// leader decides this, from the beats that a majority of the voters hear:
// the host cannot tell whether the leader is about to fence it. Once the
// host is fencing, nothing stops the operation, not even a late beat.
var phaseChanges = map[HostPhase]map[HostPhase]mover{
	Joining:    {Passive: byLeaderOrCLI, Active: byLeaderOrCLI},
	Passive:    {Active: byLeaderOrCLI},
	Active:     {Draining: byLeaderOrCLI, Failed: byLeaderOrCLI},
	Draining:   {Active: byLeaderOrCLI, Failed: byLeaderOrCLI},
	Failed:     {Active: byLeaderOrCLI, Fenced: byLeaderOrCLI}, // fenced: a user's fence-confirm
	Fencing:    {Fenced: byLeaderOrCLI, Failed: byLeaderOrCLI},
	Fenced:     {Recovering: byTheHost, Disabled: byLeaderOrCLI},
	Recovering: {Active: byTheHost, Failed: byLeaderOrCLI},
	Disabled:   {Fenced: byLeaderOrCLI}, // a user's host enable
}

// FlapWindow and FlapLimit are the flapping brake: a host fenced FlapLimit
// times within FlapWindow stays off (disabled) until a user enables it. A
// host that fails again and again would otherwise move its stacks back and
// forth, each time with a failover.
const (
	FlapWindow = time.Hour
	FlapLimit  = 3
)

// Flapping tells whether the host was fenced FlapLimit times within
// FlapWindow before now.
func (h *Host) Flapping(now time.Time) bool {
	recent := 0
	for _, at := range h.FencedAt {
		if now.Sub(at) < FlapWindow {
			recent++
		}
	}
	return recent >= FlapLimit
}

// LastFenced is when the host was last fenced, or zero if it was not fenced
// since a user enabled it. FencedAt keeps each fence until the next fence
// drops those older than FlapWindow, so the time may be older than
// FlapWindow.
func (h *Host) LastFenced() time.Time {
	if len(h.FencedAt) == 0 {
		return time.Time{}
	}
	return h.FencedAt[len(h.FencedAt)-1]
}

// fence records that the host's server is off. It records the time for the
// flapping brake and marks the host's copies as no longer synced (unsync).
func (s *State) fence(host *Host, at time.Time) {
	host.FencedAt = slices.DeleteFunc(host.FencedAt, func(t time.Time) bool { return at.Sub(t) >= FlapWindow })
	host.FencedAt = append(host.FencedAt, at)
	s.unsync(host)
}

// unsync marks the copies of a failed host as no longer synced. The other
// copies go on without them, and once the host is back, DRBD must catch
// them up first. Until the host reports a copy synced again, no stack fails
// over or moves to that copy, and a recovering host does not become active.
// A failed host's own stacks keep running (KeepsStacks), whatever its
// copies' flag says.
func (s *State) unsync(host *Host) {
	for _, id := range slices.Sorted(maps.Keys(s.Volumes)) {
		volume := s.Volumes[id]
		for index, volumeCopy := range volume.Copies {
			if volumeCopy.Host == host.ID && !volumeCopy.Tiebreaker && volumeCopy.Synced {
				volume.Copies[index].Synced = false
				s.changed(resource.KindVolume, &volume.Meta)
			}
		}
	}
}

// checkCaughtUp refuses to make a recovering host active while one of its
// copies with a disk is not synced: it would take stacks it cannot run
// with their data.
func (s *State) checkCaughtUp(host *Host) error {
	for _, id := range slices.Sorted(maps.Keys(s.Volumes)) {
		for _, volumeCopy := range s.Volumes[id].Copies {
			if volumeCopy.Host == host.ID && !volumeCopy.Tiebreaker && !volumeCopy.Synced {
				return fmt.Errorf("host %s: its copy of volume %s is not synced yet", host.Name, id)
			}
		}
	}
	return nil
}

// KeepsStacks tells whether a host in this phase runs the stacks placed on
// it. A failed host keeps running them until it is powered off, because no
// other host runs them before that (fencing). A fenced host runs none until
// it is active again, because they may run elsewhere by then. The section
// Host phases in docs/developing/cluster.md has a table of what each phase
// means where.
func KeepsStacks(phase HostPhase) bool {
	return RunsStacks(phase) || phase == Failed || phase == Fencing
}

// RunsStacks tells whether a host in this phase runs stacks: a draining
// host keeps those it has.
func RunsStacks(phase HostPhase) bool {
	return phase == Active || phase == Draining
}

// Up tells whether a host in this phase runs and is heard: it runs stacks,
// or it catches its copies up after a fence. The leader fails such a host
// when its beats stop, and only its copies resync.
func Up(phase HostPhase) bool {
	return RunsStacks(phase) || phase == Recovering
}

// MinHAHosts is how many hosts run stacks in a cluster that is highly
// available: three voters, so that a majority survives the loss of one
// host.
const MinHAHosts = 3

// hostsRunningStacks counts the hosts that run stacks. The hosts in
// leftOut are not counted.
func (s *State) hostsRunningStacks(leftOut ...HostID) int {
	count := 0
	for _, other := range s.Hosts {
		if !slices.Contains(leftOut, other.ID) && RunsStacks(other.Phase) {
			count++
		}
	}
	return count
}

// CheckRemoval tells whether the state would remove the host
// (checkRemoval). The API asks before the host hands over its lead.
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

// CheckLeaving tells whether a host may leave (checkMayGo). The API asks
// when CheckRemoval refuses a host because of its stacks and copies; these
// then move off the host first (SetHostPhase.Leaving).
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

// checkRemoval refuses to remove a host that the cluster cannot do without
// (checkMayGo). It also refuses a host that runs stacks while a stack or a
// volume copy is still on it, because they would be lost with it. Such a
// host leaves instead (SetHostPhase.Leaving): its stacks and copies move
// off it, and then the host goes.
func (s *State) checkRemoval(host *Host) error {
	if err := s.checkMayGo(host); err != nil || !RunsStacks(host.Phase) {
		return err
	}
	return s.checkEmpty(host)
}

// checkMayGo refuses to let a host go that the cluster cannot do without:
//   - the last host that runs stacks: the cluster would go on without
//     placement, ingress or alerts, and nothing brings a removed host back;
//   - once the cluster is highly available, any host, even a dead one,
//     whose removal would leave fewer than MinHAHosts hosts that run
//     stacks; a replacement joins first. A cluster never goes back below
//     three hosts. The one way out is clowderd recover --power-off, which
//     phase 2 brings for a quorum that is lost for good.
//
// Before the cluster is highly available, a host that runs no stacks
// (failed, fenced) may always go, so that a dead host can be given up.
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
	case RunsStacks(host.Phase) && remaining == 0:
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

// remove marks the host as removed. It revokes the host's certificate,
// forgets its retired copies, and asks every remaining host to forget it as
// a DRBD peer (forgetPeer). The host's ID is free once all of them have
// done so.
func (s *State) remove(host *Host, at time.Time) {
	host.Phase, host.Voter = Removed, false
	s.revoke(host.CertificateSerial, host.CertificateNotAfter, at)
	// nobody is left to remove the host's retired copies
	s.Retired = slices.DeleteFunc(s.Retired, func(c RetiredCopy) bool { return c.Host == host.ID })
	s.forgetRetiredMinors()
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

// forgot takes a host off the list of hosts that still have to forget the
// peer. Once the list is empty, it takes the peer off Forgetting.
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
	if err := s.checkUnlocked(HostLock(host.ID), payload.Operation); err != nil {
		return nil, err
	}
	who, allowed := phaseChanges[host.Phase][payload.Phase]
	switch {
	case payload.Leaving && payload.Phase != Draining:
		return nil, fmt.Errorf("host %s: only a draining host leaves", host.Name)
	case payload.Leaving:
		// a remove of a host that still has stacks or copies: it may go
		// as soon as they have moved
		if err := s.checkMayGo(host); err != nil {
			return nil, err
		}
		allowed = allowed || host.Phase == Draining
	case payload.Phase == Removed:
		if err := s.checkRemoval(host); err != nil {
			return nil, err
		}
		who, allowed = byLeaderOrCLI, true
	}
	if !allowed {
		return nil, fmt.Errorf("host %s cannot go from %s to %s", host.Name, host.Phase, payload.Phase)
	}
	fromHost := IsHostActor(cmd.Actor)
	switch {
	case who == byTheHost && cmd.Actor != HostActor(host.ID):
		return nil, fmt.Errorf("only host %s itself reports %s", host.Name, payload.Phase)
	case who == byLeaderOrCLI && fromHost:
		return nil, fmt.Errorf("%s cannot move host %s to %s; only the leader or the CLI does", cmd.Actor, host.Name, payload.Phase)
	case host.Phase == Recovering && payload.Phase == Active:
		if err := s.checkCaughtUp(host); err != nil {
			return nil, err
		}
	case host.Phase == Fencing && (s.FenceOf(host.ID) == nil || s.FenceOf(host.ID).ID != payload.Operation):
		return nil, fmt.Errorf("host %s is fencing; only its fence-host operation moves it on", host.Name)
	}
	return func() {
		if payload.Phase == Removed {
			s.remove(host, cmd.At)
			return
		}
		s.enterPhase(host, payload, cmd.At)
	}, nil
}

// enterPhase moves a host to a phase that applySetHostPhase allowed, and
// updates what goes with the move: the fence's record and copies, the
// user's remove, the requests to move stacks to the host, and the
// cluster's HA.
func (s *State) enterPhase(host *Host, payload SetHostPhase, at time.Time) {
	switch {
	case payload.Phase == Fenced && host.Phase == Disabled:
		// a user enabled it: the brake starts counting anew
		host.FencedAt = nil
	case payload.Phase == Fenced:
		s.fence(host, at)
		// in the same command, so the operation waits for exactly these
		if operation := s.Operations[payload.Operation]; operation != nil && operation.Fence != nil {
			operation.Fence.Stacks = s.stacksOn(host.ID)
		}
	case payload.Phase == Failed:
		s.unsync(host)
	}
	// an undrain takes the remove back; a failure keeps it, so the host
	// leaves once it is back
	undrain := host.Phase == Draining && payload.Phase == Active
	host.Leaving = payload.Leaving || (host.Leaving && !undrain)
	host.Phase = payload.Phase
	if payload.Phase != Active || payload.Leaving {
		s.forgetMovesTo(host.ID)
	}
	if s.hostsRunningStacks() >= MinHAHosts {
		s.HA, s.RecoveredAt = true, time.Time{}
	}
}

// SetHostVoter records whether a host votes in Raft, after the leader
// changed Raft's configuration (the controller's members loop). Raft
// decides; the state only mirrors it.
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

// ReportPeerForgotten is a host reporting that it forgot a removed host as
// a DRBD peer (drbdsetup forget-peer on each of its resources), so that a
// new host may take the ID.
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
// active host with a checked server can hold it, because Hetzner routes
// the address to a server by its ID.
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
	case !host.ServerChecked:
		return nil, fmt.Errorf("host %s has no checked server", host.Name)
	}
	id := payload.Host
	return func() { s.Ingress = &id }, nil
}

// ReportHost is an agent reporting what it is and how it is doing: the
// version it runs, its memory and CPUs, its thin pools (by pool name, "vg/thin"),
// whether Docker answers and how full the root file system is. The leader
// places stacks by it and raises alerts from it. Agents send it when
// something has changed, not on every check, so every value is one that
// seldom changes: a bool, a whole percent.
type ReportHost struct {
	Host            HostID          `json:"host"`
	Version         string          `json:"version"`
	Capacity        Resources       `json:"capacity"`
	Pools           map[string]Pool `json:"pools,omitempty"`
	DockerDown      bool            `json:"docker_down,omitempty"`
	RootUsedPercent int             `json:"root_used_percent,omitempty"`
	SplitBrain      []string        `json:"split_brain,omitempty"`
	NoQuorum        []string        `json:"no_quorum,omitempty"`
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
		host.SplitBrain, host.NoQuorum = payload.SplitBrain, payload.NoQuorum
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

// stacksOn returns the IDs of the stacks placed on a host, sorted.
func (s *State) stacksOn(id HostID) []string {
	var stacks []string
	for _, stackID := range slices.Sorted(maps.Keys(s.Stacks)) {
		if placed := s.Stacks[stackID].Host; placed != nil && *placed == id {
			stacks = append(stacks, stackID)
		}
	}
	return stacks
}

// CaughtUp tells whether every copy with a disk that the host has is
// synced: a recovering host is active only then (checkCaughtUp).
func (s *State) CaughtUp(id HostID) bool {
	host, ok := s.Hosts[id]
	return ok && s.checkCaughtUp(host) == nil
}
