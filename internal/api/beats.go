package api

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/webcodingcat/clowder/internal/state"
)

// Beats records the beats that this agent receives from hosts
// (controller.Heard). The leader asks the voters for them before it treats
// a host as failed.
type Beats interface {
	Beat(host state.HostID)
	Silence(host state.HostID) time.Duration
}

// Silence is a voter's answer to how long it has not heard from a host. The
// JSON holds the duration in nanoseconds, as time.Duration encodes it.
type Silence struct {
	SilentFor time.Duration `json:"silent_for"`
}

// localBeat records a beat for the host that the caller's certificate
// names.
func (s *Server) localBeat(w http.ResponseWriter, r *http.Request) {
	s.config.Beats.Beat(state.HostID(identityOf(r).HostID))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) localSilence(w http.ResponseWriter, r *http.Request) {
	number, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("host ID %q is not a number", r.PathValue("id")))
		return
	}
	writeJSON(w, http.StatusOK, Silence{SilentFor: s.config.Beats.Silence(state.HostID(number))})
}
