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

	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/resource"
	"github.com/webcodingcat/clowder/internal/state"
)

// ResolveRequest is the body of POST
// /v1/volumes/{namespace}/{name}/resolve-splitbrain: the host whose copy
// keeps its data.
type ResolveRequest struct {
	Keep string `json:"keep"`
}

// ResolveAnswer says which hosts connected their copy again, and which of
// them gave their changes up for the kept copy's data.
type ResolveAnswer struct {
	Hosts []ResolvedCopy `json:"hosts"`
}

// ResolvedCopy is what one host did.
type ResolvedCopy struct {
	Host string `json:"host"`
	// Discarded says the copy gave its changes since the split up
	Discarded bool `json:"discarded"`
	// Connected is false when the copy stood alone from no peer: nothing
	// to do there
	Connected bool `json:"connected"`
}

// internalResolve is the body of the internal route a host's copy is
// resolved by.
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
// again: first those whose copies with a disk give their changes up, then
// the host to keep and the tiebreaker, which keep their data. It refuses
// while the volume's stack runs elsewhere than on the host to keep: that
// copy is primary, and its data cannot be discarded. Nothing passes Raft:
// the hosts' DRBD holds the split, and the state does not know of it but
// from their reports.
func (s *Server) resolveSplitBrain(w http.ResponseWriter, r *http.Request) {
	namespace, name := r.PathValue("namespace"), r.PathValue("name")
	if err := state.Authorize(actorOf(identityOf(r)), state.VerbResolve, resource.KindVolume, namespace); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	var request ResolveRequest
	if err := decodeStrict(r.Body, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	id := names.ID(namespace, name)
	copies, status, err := s.resolvingCopies(id, request.Keep)
	if err != nil {
		writeError(w, status, err)
		return
	}
	var answer ResolveAnswer
	for _, resolving := range copies {
		connected, err := s.resolveOn(r.Context(), resolving.host, id, resolving.discard)
		if err != nil {
			writeError(w, http.StatusBadGateway, fmt.Errorf("resolve the copy on %s: %w", resolving.name, err))
			return
		}
		answer.Hosts = append(answer.Hosts, ResolvedCopy{Host: resolving.name, Discarded: resolving.discard, Connected: connected})
	}
	if !slices.ContainsFunc(answer.Hosts, func(c ResolvedCopy) bool { return c.Connected }) {
		writeError(w, http.StatusConflict, fmt.Errorf("volume %s: no copy stands alone from a peer", id))
		return
	}
	writeJSON(w, http.StatusOK, answer)
}

// resolvingCopies are the volume's copies in the order they are resolved,
// or why the request is refused, with its HTTP status.
func (s *Server) resolvingCopies(id, keep string) ([]resolvingCopy, int, error) {
	var copies []resolvingCopy
	status, err := http.StatusOK, error(nil)
	s.config.Cluster.Read(func(current *state.State) {
		volume, ok := current.Volumes[id]
		if !ok {
			status, err = http.StatusNotFound, fmt.Errorf("no volume %s", id)
			return
		}
		keepIndex := slices.IndexFunc(volume.Copies, func(c state.VolumeCopy) bool {
			host := current.Hosts[c.Host]
			return host != nil && host.Name == keep && !c.Tiebreaker
		})
		if volume.Minor == nil || keepIndex < 0 {
			status, err = http.StatusBadRequest, fmt.Errorf("%q holds no copy of volume %s with DRBD and a disk", keep, id)
			return
		}
		for _, stackID := range slices.Sorted(maps.Keys(current.Stacks)) {
			stack := current.Stacks[stackID]
			if stack.Namespace == volume.Namespace && stack.Host != nil && *stack.Host != volume.Copies[keepIndex].Host &&
				slices.Contains(stack.Spec.VolumeNames(), volume.Name) {
				status, err = http.StatusConflict, fmt.Errorf("stack %s runs on %s with the volume, whose copy there "+
					"cannot give its data up; keep that copy, or move the stack to %s first",
					stackID, current.Hosts[*stack.Host].Name, keep)
				return
			}
		}
		var kept []resolvingCopy
		for _, volumeCopy := range volume.Copies {
			resolving := resolvingCopy{host: volumeCopy.Host, name: current.Hosts[volumeCopy.Host].Name}
			if volumeCopy.Host != volume.Copies[keepIndex].Host && !volumeCopy.Tiebreaker {
				resolving.discard = true
				copies = append(copies, resolving)
			} else {
				kept = append(kept, resolving)
			}
		}
		copies = append(copies, kept...)
	})
	return copies, status, err
}

// resolveOn resolves the copy on a host, this one or another; connected is
// false when it stood alone from no peer.
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
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, ResolvedCopy{Discarded: request.Discard, Connected: connected})
}
