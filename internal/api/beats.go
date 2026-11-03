package api

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/webcodingcat/clowder/internal/state"
)

// Beats keeps the hosts' beats on this agent (controller.Heard is the real
// one): every agent beats to every voter, and the leader asks the voters
// how long they have not heard a host before it takes it for failed.
type Beats interface {
	Beat(host state.HostID)
	Silence(host state.HostID) time.Duration
}

// Silence is a voter's answer to how long it has not heard a host; JSON
// has the duration in nanoseconds, as time.Duration encodes.
type Silence struct {
	SilentFor time.Duration `json:"silent_for"`
}

// localBeat records the caller's beat: the host its certificate names.
func (s *Server) localBeat(w http.ResponseWriter, r *http.Request) {
	s.config.Beats.Beat(state.HostID(identityOf(r).HostID))
	w.WriteHeader(http.StatusNoContent)
}

// localSilence answers how long this agent has not heard a host.
func (s *Server) localSilence(w http.ResponseWriter, r *http.Request) {
	number, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("host ID %q is not a number", r.PathValue("id")))
		return
	}
	writeJSON(w, http.StatusOK, Silence{SilentFor: s.config.Beats.Silence(state.HostID(number))})
}

// Beat tells a voter that this host is alive (the agent's beats).
func (s *Server) Beat(ctx context.Context, voter state.HostID) error {
	url := "https://" + s.config.APIAddress(voter) + "/v1/internal/beats"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return fmt.Errorf("beat to host %d: %w", voter, err)
	}
	request.Header.Set(VersionHeader, s.config.Version)
	if err := send(s.forwarder, request, nil); err != nil {
		return fmt.Errorf("beat to host %d: %w", voter, err)
	}
	return nil
}

// AskSilence asks a voter how long it has not heard a host
// (controller.AskSilence).
func (s *Server) AskSilence(ctx context.Context, voter, host state.HostID) (time.Duration, error) {
	url := fmt.Sprintf("https://%s/v1/internal/beats/%d", s.config.APIAddress(voter), host)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, fmt.Errorf("ask host %d about host %d: %w", voter, host, err)
	}
	request.Header.Set(VersionHeader, s.config.Version)
	var answer Silence
	if err := send(s.forwarder, request, &answer); err != nil {
		return 0, fmt.Errorf("ask host %d about host %d: %w", voter, host, err)
	}
	return answer.SilentFor, nil
}
