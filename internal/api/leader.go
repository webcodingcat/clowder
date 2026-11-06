package api

import (
	"errors"
	"net/http"

	"github.com/webcodingcat/clowder/internal/state"
)

// KnownLeader is an agent's answer to which host leads its cluster.
type KnownLeader struct {
	Leader state.HostID `json:"leader"`
}

// knownLeader serves RecoverCaller.KnowsLeader. Until its heartbeat times
// out, a follower may name a leader that is gone; recover then refuses,
// which is the safe side.
func (s *Server) knownLeader(w http.ResponseWriter, r *http.Request) {
	if s.config.Cluster.IsLeader() {
		if err := s.config.Cluster.VerifyLeader(r.Context()); err != nil {
			writeError(w, http.StatusServiceUnavailable, err)
			return
		}
		writeJSON(w, http.StatusOK, KnownLeader{Leader: s.config.HostID})
		return
	}
	leader, ok := s.config.Cluster.Leader()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, errors.New("no leader known"))
		return
	}
	writeJSON(w, http.StatusOK, KnownLeader{Leader: leader})
}
