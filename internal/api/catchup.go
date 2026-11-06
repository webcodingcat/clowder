package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
)

// CommitIndex is the leader's answer to how far the log is committed.
type CommitIndex struct {
	Index uint64 `json:"index"`
}

// localCommitIndex answers with this leader's commit index, once a majority
// has confirmed its lead. A leader cut off from the others would name an
// index that the cluster has already gone past.
func (s *Server) localCommitIndex(w http.ResponseWriter, r *http.Request) {
	if !s.config.Cluster.IsLeader() {
		writeError(w, http.StatusServiceUnavailable, errors.New("not the leader; try again"))
		return
	}
	if err := s.config.Cluster.VerifyLeader(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, err)
		return
	}
	writeJSON(w, http.StatusOK, CommitIndex{Index: s.config.Cluster.CommitIndex()})
}

// CatchUpWithLeader returns once this agent's state holds everything the
// leader had committed when it was asked. A host that was away (fenced,
// rebooted) starts its stacks only after this returns: a stack that moved
// to another host while it was away must not start here from an old state.
func (s *Server) CatchUpWithLeader(ctx context.Context) error {
	id, here, err := s.waitForLeader(ctx)
	switch {
	case err != nil:
		return err
	case here:
		return nil // IsLeader holds only once its barrier applied the log (raftnode.Node)
	}
	url := "https://" + s.config.APIAddress(id) + "/v1/internal/commit-index"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("ask the leader (host %d) for its commit index: %w", id, err)
	}
	request.Header.Set(VersionHeader, s.config.Version)
	var answer CommitIndex
	if err := send(s.forwarder, request, &answer); err != nil {
		return fmt.Errorf("ask the leader (host %d) for its commit index: %w", id, err)
	}
	return s.config.Cluster.WaitCommitted(ctx, answer.Index)
}
