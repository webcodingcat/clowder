package state

import (
	"errors"
	"fmt"
	"maps"
	"time"
)

// A host joins with a one-time token (internal/jointoken): a user makes one
// (add-join-token), and the leader adds the host with it (add-host), which
// uses the token up in the same command. The state keeps only the token's
// hash, and the handlers check its expiry against the command's time.

// JoinToken is an open join token.
type JoinToken struct {
	// Hash is jointoken.Token.Hash: SHA-256 of the secret, in hex.
	Hash    string    `json:"hash"`
	Expires time.Time `json:"expires"`
}

// The limits of join tokens. A token lives at least a minute, so a host
// that boots slowly still makes it, and at most a day: it sits in the user
// data and the OpenTofu state, where it must be worthless soon.
const (
	MinJoinTokenTTL     = time.Minute
	MaxJoinTokenTTL     = 24 * time.Hour
	DefaultJoinTokenTTL = time.Hour
	// MaxJoinTokens bounds the open tokens: more than one per possible
	// host is never needed.
	MaxJoinTokens = MaxHosts
)

// joinTokenKept is how long an expired token stays, so that a join with it
// is told it expired rather than that it is unknown.
const joinTokenKept = time.Hour

// AddJoinToken opens a join token for TTL from the command's time.
type AddJoinToken struct {
	ID   string        `json:"id"`
	Hash string        `json:"hash"`
	TTL  time.Duration `json:"ttl"`
}

func applyAddJoinToken(s *State, cmd Command, payload AddJoinToken) (func(), error) {
	switch {
	case !IsUserActor(cmd.Actor):
		return nil, fmt.Errorf("%s cannot make a join token; only a user does", cmd.Actor)
	case payload.ID == "":
		return nil, errors.New("the join token has no ID")
	case len(payload.Hash) != 64:
		return nil, errors.New("the join token's hash is no SHA-256 in hex")
	case payload.TTL < MinJoinTokenTTL || payload.TTL > MaxJoinTokenTTL:
		return nil, fmt.Errorf("a join token lives %s to %s, not %s", MinJoinTokenTTL, MaxJoinTokenTTL, payload.TTL)
	}
	if _, ok := s.JoinTokens[payload.ID]; ok {
		return nil, fmt.Errorf("join token %s exists already", payload.ID)
	}
	open := 0
	for _, token := range s.JoinTokens {
		if cmd.At.Before(token.Expires) {
			open++
		}
	}
	if open >= MaxJoinTokens {
		return nil, fmt.Errorf("%d join tokens are open already, the most there may be", open)
	}
	return func() {
		s.dropExpiredTokens(cmd.At)
		s.JoinTokens[payload.ID] = JoinToken{Hash: payload.Hash, Expires: cmd.At.Add(payload.TTL)}
	}, nil
}

// dropExpiredTokens drops the tokens that expired more than joinTokenKept
// before at, by the command's time, so every agent drops the same. The list
// changes only where it is called, with each add-join-token and add-host.
func (s *State) dropExpiredTokens(at time.Time) {
	maps.DeleteFunc(s.JoinTokens, func(_ string, token JoinToken) bool {
		return at.Sub(token.Expires) > joinTokenKept
	})
}

// ErrJoinTokenRefused is the answer to a token that does not open: unknown,
// used or expired. Whoever asks with the token's ID is told which; the ID
// alone opens nothing, and the 64 bits of a guessed one rarely name a
// token.
var ErrJoinTokenRefused = errors.New("the join token does not open")

// CheckJoinToken refuses a token that is unknown, used up or expired at at.
// The leader checks the token's hash before it proposes the join; add-host
// checks the rest again, at the command's time.
func (s *State) CheckJoinToken(id string, at time.Time) (JoinToken, error) {
	token, ok := s.JoinTokens[id]
	switch {
	case !ok:
		return JoinToken{}, fmt.Errorf("%w: it is unknown, or a host joined with it", ErrJoinTokenRefused)
	case !at.Before(token.Expires):
		return JoinToken{}, fmt.Errorf("%w: it expired at %s", ErrJoinTokenRefused, token.Expires.Format(time.RFC3339))
	}
	return token, nil
}

// AddHost adds a host that joined with a token. The leader checked the
// token's hash and the host's server with Hetzner, issued the
// host's certificate and chose its ID (FreeHostID).
type AddHost struct {
	// Token is the ID of the join token, which the command uses up.
	Token string `json:"token"`
	Host  Host   `json:"host"`
}

func applyAddHost(s *State, cmd Command, payload AddHost) (func(), error) {
	host := payload.Host
	if cmd.Actor != LeaderActor {
		return nil, fmt.Errorf("%s cannot add a host; only the leader does", cmd.Actor)
	}
	if _, err := s.CheckJoinToken(payload.Token, cmd.At); err != nil {
		return nil, err
	}
	if host.ID < 0 || host.ID >= MaxHosts {
		return nil, fmt.Errorf("host ID %d is not between 0 and %d", host.ID, MaxHosts-1)
	}
	if !s.HostIDFree(host.ID) {
		return nil, fmt.Errorf("host ID %d is taken", host.ID)
	}
	if err := s.CheckJoiningHost(host); err != nil {
		return nil, err
	}
	// the Floating IP and fencing act on the server, so a host without a
	// checked one would be a host the cluster cannot fail over from
	if !host.ServerChecked {
		return nil, fmt.Errorf("host %s has no checked server", host.Name)
	}
	return func() {
		s.dropExpiredTokens(cmd.At)
		delete(s.JoinTokens, payload.Token)
		host.Phase, host.Voter = Joining, false
		host.Version, host.Capacity, host.Pools = "", Resources{}, nil
		s.Hosts[host.ID] = &host
	}, nil
}

// HostIDFree tells whether a joining host may get the ID: no host has it,
// or a removed one that every other host forgot as a DRBD peer. DRBD's
// node-id is the host ID, and a peer's slot in the metadata outlives it
// until forget-peer.
func (s *State) HostIDFree(id HostID) bool {
	host, taken := s.Hosts[id]
	if !taken {
		return true
	}
	_, forgetting := s.Forgetting[id]
	return host.Phase == Removed && !forgetting
}

// FreeHostID is the lowest ID a joining host may get; false if the
// cluster is full.
func (s *State) FreeHostID() (HostID, bool) {
	for id := range HostID(MaxHosts) {
		if s.HostIDFree(id) {
			return id, true
		}
	}
	return 0, false
}

// CheckJoiningHost refuses a joining host that lacks what a host must
// bring, whose underlay is outside the private network, whose
// certificate's serial is in use, or whose name, underlay or server
// another host has, unless that host is removed: two hosts of one server
// would fence each other's. The leader asks it before it asks Hetzner,
// add-host again.
func (s *State) CheckJoiningHost(host Host) error {
	if err := checkNewHost(host); err != nil {
		return err
	}
	if err := checkUnderlay(s.Config.Spec, &host); err != nil {
		return err
	}
	if s.serialInUse(host.CertificateSerial) {
		return fmt.Errorf("serial %s is used already", host.CertificateSerial)
	}
	for _, other := range s.Hosts {
		switch {
		case other.Phase == Removed:
		case other.Name == host.Name:
			return fmt.Errorf("a host is named %s already", host.Name)
		case other.Underlay == host.Underlay:
			return fmt.Errorf("host %s has the underlay %s already", other.Name, host.Underlay)
		case other.ServerID == host.ServerID:
			return fmt.Errorf("host %s has the server %d already", other.Name, host.ServerID)
		}
	}
	return nil
}

// ConfirmServer records that Hetzner confirmed a host's server: the first
// host's, which init took from the metadata service unchecked, once
// cluster.yaml brought the project's token. ServerID is the server the
// leader checked; it must still be the host's.
type ConfirmServer struct {
	Host     HostID `json:"host"`
	ServerID int64  `json:"server_id"`
}

func applyConfirmServer(s *State, cmd Command, payload ConfirmServer) (func(), error) {
	host, ok := s.Hosts[payload.Host]
	switch {
	case cmd.Actor != LeaderActor:
		return nil, fmt.Errorf("%s cannot confirm a server; only the leader does", cmd.Actor)
	case !ok:
		return nil, fmt.Errorf("no host %d", payload.Host)
	case host.Phase == Removed:
		return nil, fmt.Errorf("host %s is removed", host.Name)
	case host.ServerID != payload.ServerID:
		return nil, fmt.Errorf("host %s has the server %d, not %d", host.Name, host.ServerID, payload.ServerID)
	case host.ServerChecked:
		return nil, errUnchanged
	}
	for _, other := range s.Hosts {
		if other.ID != host.ID && other.Phase != Removed && other.ServerID == host.ServerID {
			return nil, fmt.Errorf("hosts %s and %s have the same server %d", other.Name, host.Name, host.ServerID)
		}
	}
	return func() { host.ServerChecked = true }, nil
}
