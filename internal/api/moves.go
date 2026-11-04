package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/webcodingcat/clowder/internal/state"
)

// Volumes is what the API needs of this host's volume copies.
type Volumes interface {
	// Progress is what each copy here differs by from a peer, by volume
	// ID, in bytes: while it resyncs, what is left.
	Progress() map[string]int64
	// ResolveSplitBrain connects the copy here of a volume, by ID, again to
	// the peers it stands alone from; with discard it takes their data.
	// connected is false when it stood alone from none.
	ResolveSplitBrain(ctx context.Context, volumeID string, discard bool) (connected bool, err error)
}

// moveStack commits request-move for a host named by the user; the
// placement starts the move once the stack fits there.
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

// leaves is the phase change for a remove of a host that still has stacks
// or volume copies: it drains and leaves (SetHostPhase.Leaving), and the
// leader removes it once they moved off. Nil for a host the state would
// remove at once, or refuse anyway.
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

// localProgress answers how far this host's copies are (Volumes).
func (s *Server) localProgress(w http.ResponseWriter, _ *http.Request) {
	if s.config.Volumes == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("this agent keeps no volumes"))
		return
	}
	writeJSON(w, http.StatusOK, s.config.Volumes.Progress())
}

// Progress asks a host how far its copies are (controller.Progress).
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
