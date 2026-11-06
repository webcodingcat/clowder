package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/webcodingcat/clowder/internal/state"
)

// Volumes is what the API needs from this host's volume copies.
type Volumes interface {
	// Progress returns, by volume ID, how many bytes each copy on this
	// host differs from a peer. While a copy resyncs, this is what is left.
	Progress() map[string]int64
	// ResolveSplitBrain connects this host's copy of a volume (by ID)
	// again to the peers it is cut off from. With discard, the copy takes
	// their data. connected is false when none of its peers was StandAlone.
	ResolveSplitBrain(ctx context.Context, volumeID string, discard bool) (connected bool, err error)
}

// moveStack only commits the request; the placement starts the move once
// the stack fits on that host.
func (s *Server) moveStack(w http.ResponseWriter, r *http.Request) {
	var request MoveRequest
	if err := decodeStrict(r.Body, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	to, found := state.HostID(0), false
	s.config.Cluster.Read(func(current *state.State) {
		for id, host := range current.Hosts {
			if host.Name == request.To && host.Phase != state.Removed {
				to, found = id, true
				break
			}
		}
	})
	if !found {
		writeError(w, http.StatusNotFound, fmt.Errorf("no host %q", request.To))
		return
	}
	s.commitRequest(w, r, state.KindRequestMove, state.RequestMove{
		Namespace: r.PathValue("namespace"), Name: r.PathValue("name"), To: to,
	})
}

// leaves returns the phase change for removing a host that still has
// stacks or volume copies: it drains, and the leader removes it once they
// have moved. It returns nil for a host that the state would remove at
// once, or would refuse to remove anyway.
func (s *Server) leaves(id state.HostID) *state.SetHostPhase {
	var leave *state.SetHostPhase
	s.config.Cluster.Read(func(current *state.State) {
		host, ok := current.Hosts[id]
		if !ok || !errors.Is(current.CheckRemoval(id), state.ErrNotEmpty) || current.CheckLeaving(id) != nil {
			return
		}
		leave = &state.SetHostPhase{Host: id, Phase: state.Draining, From: host.Phase, Leaving: true}
	})
	return leave
}

func (s *Server) localProgress(w http.ResponseWriter, _ *http.Request) {
	if s.config.Volumes == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("this agent keeps no volumes"))
		return
	}
	writeJSON(w, http.StatusOK, s.config.Volumes.Progress())
}

// Progress asks a host how far its copies have synced
// (controller.Progress).
func (s *Server) Progress(ctx context.Context, host state.HostID) (map[string]int64, error) {
	url := "https://" + s.config.APIAddress(host) + "/v1/internal/volumes/progress"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("ask host %d how far its copies are: %w", host, err)
	}
	request.Header.Set(VersionHeader, s.config.Version)
	var progress map[string]int64
	if err := send(s.forwarder, request, &progress); err != nil {
		return nil, fmt.Errorf("ask host %d how far its copies are: %w", host, err)
	}
	return progress, nil
}
