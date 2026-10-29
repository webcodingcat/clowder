package state

import (
	"errors"
	"fmt"
)

// Bootstrap starts the cluster: the keys init made and the first host.
type Bootstrap struct {
	Keys    ClusterKeys `json:"keys"`
	JoinKey Sealed      `json:"join_key"`
	Host    Host        `json:"host"`
}

func applyBootstrap(s *State, _ Command, payload Bootstrap) error {
	switch {
	case s.Keys != nil:
		return errors.New("the cluster is bootstrapped already")
	case len(payload.Keys.CACertificate) == 0 || len(payload.Keys.CAKey) == 0 || len(payload.Keys.AgeIdentity) == 0:
		return errors.New("the keys are incomplete")
	case len(payload.JoinKey) == 0:
		return errors.New("the bootstrap key is missing")
	case payload.Host.ID != 0:
		return fmt.Errorf("the first host has ID 0, not %d", payload.Host.ID)
	}
	if err := checkNewHost(payload.Host); err != nil {
		return err
	}
	host := payload.Host
	host.Voter, host.Phase = true, Active
	keys := payload.Keys
	s.Keys = &keys
	s.JoinKeys = []Sealed{payload.JoinKey}
	s.Hosts[host.ID] = &host
	return nil
}

// AddHost adds a host that joins; the leader has picked its free ID.
type AddHost struct {
	Host Host `json:"host"`
}

func applyAddHost(s *State, _ Command, payload AddHost) error {
	host := payload.Host
	if host.ID < 0 || host.ID >= MaxHosts {
		return fmt.Errorf("host ID %d is not from 0 to %d", host.ID, MaxHosts-1)
	}
	// IDs of removed hosts stay taken until every DRBD peer has forgotten them
	if _, taken := s.Hosts[host.ID]; taken {
		return fmt.Errorf("host ID %d is taken", host.ID)
	}
	if err := checkNewHost(host); err != nil {
		return err
	}
	for _, other := range s.Hosts {
		if other.Name == host.Name && other.Phase != Removed {
			return fmt.Errorf("a host named %s is a member already", host.Name)
		}
	}
	host.Voter, host.Phase = false, Joining
	s.Hosts[host.ID] = &host
	return nil
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
	}
	return nil
}

// SetHostPhase moves a host to another phase.
type SetHostPhase struct {
	Host  HostID    `json:"host"`
	Phase HostPhase `json:"phase"`
	Voter *bool     `json:"voter,omitempty"` // changes the Raft role along with the phase
}

// mover is who may make a phase change: the leader, on its own or for a
// user, or the host itself, reporting what only it can see. A host never
// moves another host. Which actor may send which kind of command at all is
// the API's check; the state checks only what it alone can tell, whose host
// a report is about.
type mover int

const (
	byLeaderOrUser mover = iota
	byTheHost
)

// phaseChanges are the allowed moves; any phase may also go to Removed, by
// the leader.
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

func applySetHostPhase(s *State, cmd Command, payload SetHostPhase) error {
	host, ok := s.Hosts[payload.Host]
	if !ok {
		return fmt.Errorf("no host %d", payload.Host)
	}
	if host.Phase == Removed {
		return fmt.Errorf("host %s is removed", host.Name)
	}
	who, allowed := phaseChanges[host.Phase][payload.Phase]
	if payload.Phase == Removed {
		who, allowed = byLeaderOrUser, true
	}
	if !allowed {
		return fmt.Errorf("host %s cannot go from %s to %s", host.Name, host.Phase, payload.Phase)
	}
	fromHost := IsHostActor(cmd.Actor)
	switch {
	case who == byTheHost && cmd.Actor != HostActor(host.ID):
		return fmt.Errorf("only host %s itself reports %s", host.Name, payload.Phase)
	case who == byLeaderOrUser && fromHost:
		return fmt.Errorf("%s cannot move host %s to %s; only the leader does", cmd.Actor, host.Name, payload.Phase)
	}
	host.Phase = payload.Phase
	if payload.Voter != nil {
		host.Voter = *payload.Voter
	}
	if host.Phase == Removed {
		host.Voter = false
		s.Revoked[host.CertificateSerial] = cmd.At
	}
	return nil
}

// ReportHost is an agent telling what it is: the version it runs, what it
// offers after the system reserve, and its thin pools (by pool name,
// "vg/thin"). The leader places volumes by the pools. Agents send it when
// something changed, not on every health check.
type ReportHost struct {
	Host     HostID          `json:"host"`
	Version  string          `json:"version"`
	Capacity Resources       `json:"capacity"`
	Pools    map[string]Pool `json:"pools,omitempty"`
}

func applyReportHost(s *State, cmd Command, payload ReportHost) error {
	host, ok := s.Hosts[payload.Host]
	switch {
	case !ok:
		return fmt.Errorf("no host %d", payload.Host)
	case cmd.Actor != HostActor(host.ID):
		return fmt.Errorf("only host %s itself reports about itself", host.Name)
	case payload.Version == "":
		return errors.New("the version is empty")
	}
	for name, pool := range payload.Pools {
		if pool.SizeBytes <= 0 || pool.UsedBytes < 0 {
			return fmt.Errorf("host %s: pool %s with size %d and use %d", host.Name, name, pool.SizeBytes, pool.UsedBytes)
		}
	}
	host.Version, host.Capacity, host.Pools = payload.Version, payload.Capacity, payload.Pools
	return nil
}
