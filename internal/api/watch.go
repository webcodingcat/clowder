package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/webcodingcat/clowder/internal/resource"
)

// WatchEvent is one change that GET /v1/watch streams, as one JSON object
// per line. It says which resource changed (spec or status) or is gone.
// The client reads the resource itself if it needs it.
type WatchEvent struct {
	Kind            string `json:"kind"`
	Namespace       string `json:"namespace,omitempty"`
	Name            string `json:"name"`
	ResourceVersion uint64 `json:"resourceVersion"`
	Deleted         bool   `json:"deleted,omitempty"`
}

// watchWaitTimeout bounds how long a watch from an index that this agent
// has not applied yet waits for it; otherwise a follower cut off from the
// leader would wait forever. It is a variable so that the tests need not
// wait as long.
var watchWaitTimeout = 30 * time.Second

// watch streams the changes of one kind in a namespace (or all of them)
// after ?since=, the index of the client's list:
// GET /v1/watch?kind=&namespace=|allNamespaces=true&since=. Any agent
// answers from its own ring of changes (state.ChangeRing), so a client
// that listed on one agent can watch on another.
//   - since is older than what the ring holds: 410, and the client lists
//     again
//   - since is ahead of this agent: it waits at most watchWaitTimeout,
//     then 504
//   - the stream ends when ctx ends, or when the ring has dropped changes
//     that the client has not read yet (the client lists again, as after
//     410)
func (s *Server) watch(w http.ResponseWriter, r *http.Request) {
	kind, ok := resource.KindOf(r.URL.Query().Get("kind"))
	if !ok {
		writeError(w, http.StatusBadRequest, fmt.Errorf("no kind of resource %q", r.URL.Query().Get("kind")))
		return
	}
	since, err := strconv.ParseUint(r.URL.Query().Get("since"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("since= is the index of the list the watch follows"))
		return
	}
	selected, ok := selectNamespace(w, r, kind)
	if !ok {
		return
	}
	reached, cancel := context.WithTimeout(r.Context(), watchWaitTimeout)
	err = s.config.Cluster.WaitIndex(reached, since)
	cancel()
	if err != nil {
		writeError(w, http.StatusGatewayTimeout, fmt.Errorf("this agent has not reached index %d yet; try again", since))
		return
	}
	// get the channel before the changes, so that no change between the
	// read and the wait goes unseen
	changed := s.config.Cluster.Changed()
	changes, index, err := s.config.Cluster.Changes(since)
	if err != nil {
		writeError(w, http.StatusGone, err)
		return
	}
	ctx, cancel := s.streamContext(r)
	defer cancel()
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	encoder := json.NewEncoder(flushingWriter{w})
	for {
		for _, change := range changes {
			if change.Kind != kind || !selected.holds(change.Namespace) {
				continue
			}
			if err := encoder.Encode(WatchEvent{
				Kind: change.Kind, Namespace: change.Namespace, Name: change.Name,
				ResourceVersion: change.ResourceVersion, Deleted: change.Deleted,
			}); err != nil {
				return // the client is gone
			}
		}
		since = index
		select {
		case <-changed:
		case <-ctx.Done():
			return
		}
		changed = s.config.Cluster.Changed()
		if changes, index, err = s.config.Cluster.Changes(since); err != nil {
			return // the ring went on without the client: it lists again
		}
	}
}
