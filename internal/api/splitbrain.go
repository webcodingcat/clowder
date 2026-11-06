package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/state"
)

// ResolveRequest is the body of POST
// /v1/volumes/{namespace}/{name}/resolve-splitbrain: the host whose copy
// keeps its data.
type ResolveRequest struct {
	Keep string `json:"keep"`
}

// ResolveAnswer says which hosts connected their copy again, and which of
// them gave up their changes for the kept copy's data.
type ResolveAnswer struct {
	Hosts []ResolvedCopy `json:"hosts"`
}

// ResolvedCopy is what one host did.
type ResolvedCopy struct {
	Host string `json:"host"`
	// Discarded says that the copy gave up its changes since the split and
	// connected again
	Discarded bool `json:"discarded"`
	// Connected is false when none of the copy's peers was StandAlone:
	// there was nothing to do there
	Connected bool `json:"connected"`
}

// internalResolve is the body of the internal route that resolves a host's
// copy.
type internalResolve struct {
	Discard bool `json:"discard"`
}

// resolvingCopy is a host with a copy of the volume, and whether its copy
// discards its data.
type resolvingCopy struct {
	host    state.HostID
	name    string
	discard bool
}

// resolveSplitBrain asks each host with a copy of a volume to connect it
// again. First come the hosts whose copies with a disk give up their
// changes, then the host to keep and the tiebreaker, which keep their
// data. It refuses while the volume's stack runs on a host other than the
// one to keep, because that copy is primary and its data cannot be
// discarded. Nothing goes through Raft: the hosts' DRBD holds the split,
// and the state knows of it only from their reports.
func (s *Server) resolveSplitBrain(w http.ResponseWriter, r *http.Request) {
	namespace, name := r.PathValue("namespace"), r.PathValue("name")
	var request ResolveRequest
	if err := decodeStrict(r.Body, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	id := names.ID(namespace, name)
	var (
		copies []resolvingCopy
		status int
		err    error
	)
	s.config.Cluster.Read(func(current *state.State) { copies, status, err = resolveOrder(current, id, request.Keep) })
	if err != nil {
		writeError(w, status, err)
		return
	}
	var answer ResolveAnswer
	for _, resolving := range copies {
		connected, err := s.resolveOn(r.Context(), resolving.host, id, resolving.discard)
		if err != nil {
			writeError(w, http.StatusBadGateway, fmt.Errorf("resolve the copy on %s: %w (%s; running the command "+
				"again is safe)", resolving.name, err, doneSoFar(answer.Hosts)))
			return
		}
		answer.Hosts = append(answer.Hosts, ResolvedCopy{
			Host: resolving.name, Discarded: resolving.discard && connected, Connected: connected,
		})
	}
	if !slices.ContainsFunc(answer.Hosts, func(c ResolvedCopy) bool { return c.Connected }) {
		writeError(w, http.StatusConflict, fmt.Errorf("volume %s: no copy is StandAlone toward a peer", id))
		return
	}
	writeJSON(w, http.StatusOK, answer)
}

// doneSoFar says which hosts connected their copy again before one failed.
func doneSoFar(resolved []ResolvedCopy) string {
	var connected []string
	for _, done := range resolved {
		if done.Connected {
			connected = append(connected, done.Host)
		}
	}
	if len(connected) == 0 {
		return "no copy is connected again yet"
	}
	return "connected again already: " + strings.Join(connected, ", ")
}

// resolveOrder returns the volume's copies in the order in which they are
// resolved, or why the request is refused, with its HTTP status. Every
// copy's host is in the state, because the state refuses a copy on a host
// it does not know.
func resolveOrder(current *state.State, id, keep string) ([]resolvingCopy, int, error) {
	volume, ok := current.Volumes[id]
	if !ok {
		return nil, http.StatusNotFound, fmt.Errorf("no volume %s", id)
	}
	keepIndex := slices.IndexFunc(volume.Copies, func(c state.VolumeCopy) bool {
		return current.Hosts[c.Host].Name == keep && !c.Tiebreaker
	})
	if volume.Minor == nil || keepIndex < 0 {
		return nil, http.StatusBadRequest, fmt.Errorf("%q holds no copy of volume %s with DRBD and a disk", keep, id)
	}
	keptHost := volume.Copies[keepIndex].Host
	for _, stackID := range slices.Sorted(maps.Keys(current.Stacks)) {
		stack := current.Stacks[stackID]
		if stack.Namespace == volume.Namespace && stack.Host != nil && *stack.Host != keptHost &&
			slices.Contains(stack.Spec.VolumeNames(), volume.Name) {
			return nil, http.StatusConflict, fmt.Errorf("stack %s runs on %s with the volume, whose copy there "+
				"cannot give its data up; keep that copy, or move the stack to %s first",
				stackID, current.Hosts[*stack.Host].Name, keep)
		}
	}
	var discarding, keeping []resolvingCopy
	for _, volumeCopy := range volume.Copies {
		resolving := resolvingCopy{host: volumeCopy.Host, name: current.Hosts[volumeCopy.Host].Name}
		if volumeCopy.Host != keptHost && !volumeCopy.Tiebreaker {
			resolving.discard = true
			discarding = append(discarding, resolving)
		} else {
			keeping = append(keeping, resolving)
		}
	}
	return append(discarding, keeping...), http.StatusOK, nil
}

// resolveOn resolves the copy on a host, this one or another. connected is
// false when none of the copy's peers was StandAlone.
func (s *Server) resolveOn(ctx context.Context, host state.HostID, volumeID string, discard bool) (bool, error) {
	if host == s.config.HostID {
		return s.resolveHere(ctx, volumeID, discard)
	}
	namespace, name, _ := names.SplitID(volumeID)
	body, err := json.Marshal(internalResolve{Discard: discard})
	if err != nil {
		return false, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+s.config.APIAddress(host)+
		"/v1/internal/volumes/"+url.PathEscape(namespace)+"/"+url.PathEscape(name)+"/resolve-splitbrain",
		bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	request.Header.Set(VersionHeader, s.config.Version)
	var resolved ResolvedCopy
	if err := send(s.forwarder, request, &resolved); err != nil {
		return false, err
	}
	return resolved.Connected, nil
}

// resolveHere resolves this host's copy (Volumes.ResolveSplitBrain).
func (s *Server) resolveHere(ctx context.Context, volumeID string, discard bool) (bool, error) {
	if s.config.Volumes == nil {
		return false, errors.New("this agent keeps no volumes")
	}
	return s.config.Volumes.ResolveSplitBrain(ctx, volumeID, discard)
}

// localResolve resolves this host's copy for the agent that took the
// user's request.
func (s *Server) localResolve(w http.ResponseWriter, r *http.Request) {
	var request internalResolve
	if err := decodeStrict(r.Body, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	connected, err := s.resolveHere(r.Context(), names.ID(r.PathValue("namespace"), r.PathValue("name")), request.Discard)
	if err != nil {
		// no copy here, a step that runs, a primary, or DRBD's own refusal
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, ResolvedCopy{Discarded: request.Discard && connected, Connected: connected})
}
