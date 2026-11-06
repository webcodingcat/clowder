package api

import (
	"errors"
	"net/http"
)

// CommitIndex is the leader's answer to how far the log is committed.
type CommitIndex struct {
	Index uint64 `json:"index"`
}

// localCommitIndex answers with this leader's commit index, once a majority
// has confirmed that it still leads. Without that check, a leader that is
// cut off from the others could name an index that the cluster has already
// passed.
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
