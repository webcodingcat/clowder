package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/resource"
	"github.com/webcodingcat/clowder/internal/state"
)

// ContainerInfo is one container of a stack on its host.
type ContainerInfo struct {
	Service string `json:"service"`
	State   string `json:"state"`  // running, exited, ...
	Health  string `json:"health"` // healthy, unhealthy, starting, or empty
	Status  string `json:"status"` // Docker's words: "Up 3 minutes"
}

// Containers answers for the stacks that run on this host, by their Compose
// project (names.Project); the agent is the real one.
type Containers interface {
	Ps(ctx context.Context, project string) ([]ContainerInfo, error)
	// Logs writes the stack's logs to out; with follow, until ctx ends.
	Logs(ctx context.Context, project string, follow bool, out io.Writer) error
}

// stackHost finds the host a stack runs on and its Compose project there;
// ok is false (and the answer written) when there is none.
func (s *Server) stackHost(w http.ResponseWriter, r *http.Request) (state.HostID, string, bool) {
	namespace, name := r.PathValue("namespace"), r.PathValue("name")
	id := names.ID(namespace, name)
	if err := state.Authorize(actorOf(identityOf(r)), state.VerbRead, resource.KindStack, namespace); err != nil {
		writeError(w, http.StatusForbidden, err)
		return 0, "", false
	}
	var host *state.HostID
	var exists bool
	s.config.Cluster.Read(func(current *state.State) {
		if stack, ok := current.Stacks[id]; ok {
			exists, host = true, stack.Host
		}
	})
	switch {
	case !exists:
		writeError(w, http.StatusNotFound, fmt.Errorf("no stack %s", id))
		return 0, "", false
	case host == nil:
		writeError(w, http.StatusConflict, fmt.Errorf("stack %s is not placed on a host yet", id))
		return 0, "", false
	}
	return *host, names.Project(namespace, name), true
}

// stackPs lists a stack's containers, asking its host.
func (s *Server) stackPs(w http.ResponseWriter, r *http.Request) {
	host, project, ok := s.stackHost(w, r)
	if !ok {
		return
	}
	if host != s.config.HostID {
		s.relay(w, r, s.forwarder, host, "/v1/internal/stacks/"+url.PathEscape(project)+"/ps")
		return
	}
	s.ps(w, r, project)
}

// stackLogs writes a stack's logs, from its host; ?follow=true keeps going.
func (s *Server) stackLogs(w http.ResponseWriter, r *http.Request) {
	host, project, ok := s.stackHost(w, r)
	if !ok {
		return
	}
	if host != s.config.HostID {
		s.relay(w, r, s.streamer, host, "/v1/internal/stacks/"+url.PathEscape(project)+"/logs?"+r.URL.RawQuery)
		return
	}
	s.logs(w, r, project)
}

// RestartRequest is the body of POST /v1/stacks/{namespace}/{name}/restart:
// the services to restart, none for all of them. Those that depend on them
// come along (state.RestartStack).
type RestartRequest struct {
	Services []string `json:"services,omitempty"`
}

// restartStack commits restart-stack; the state checks the stack and its
// services.
func (s *Server) restartStack(w http.ResponseWriter, r *http.Request) {
	var request RestartRequest
	if err := decodeStrict(r.Body, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.write(w, r, state.KindRestartStack, state.RestartStack{
		Namespace: r.PathValue("namespace"), Name: r.PathValue("name"), Services: request.Services,
	})
}

// localProject is the Compose project of an internal route: only hosts call
// these, but the project becomes a path on this host, so it must be a
// stack's.
func localProject(w http.ResponseWriter, r *http.Request) (string, bool) {
	project := r.PathValue("project")
	if _, _, ok := names.SplitProject(project); !ok {
		writeError(w, http.StatusBadRequest, fmt.Errorf("%q is not a stack's project", project))
		return "", false
	}
	return project, true
}

func (s *Server) localPs(w http.ResponseWriter, r *http.Request) {
	if project, ok := localProject(w, r); ok {
		s.ps(w, r, project)
	}
}

func (s *Server) localLogs(w http.ResponseWriter, r *http.Request) {
	if project, ok := localProject(w, r); ok {
		s.logs(w, r, project)
	}
}

// ps answers with the containers of a stack on this host.
func (s *Server) ps(w http.ResponseWriter, r *http.Request, project string) {
	if s.config.Containers == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("this agent runs no containers"))
		return
	}
	containers, err := s.config.Containers.Ps(r.Context(), project)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err)
		return
	}
	writeJSON(w, http.StatusOK, containers)
}

// logs streams the logs of a stack on this host.
func (s *Server) logs(w http.ResponseWriter, r *http.Request, project string) {
	if s.config.Containers == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("this agent runs no containers"))
		return
	}
	follow := r.URL.Query().Get("follow") == "true"
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	writer := flushingWriter{w}
	ctx, cancel := s.streamContext(r)
	defer cancel()
	if err := s.config.Containers.Logs(ctx, project, follow, writer); err != nil {
		_, _ = fmt.Fprintf(writer, "\nclowder: the logs broke off: %v\n", err) // the status is sent already
	}
}

// flushingWriter sends each write at once, for logs that follow.
type flushingWriter struct {
	w http.ResponseWriter
}

func (f flushingWriter) Write(data []byte) (int, error) {
	n, err := f.w.Write(data)
	if flusher, ok := f.w.(http.Flusher); ok {
		flusher.Flush()
	}
	return n, err
}

// relay asks another host's internal endpoint with client and passes its
// answer on, as it streams: the forwarder for answers that must come
// within its timeout, the streamer for logs that follow.
func (s *Server) relay(w http.ResponseWriter, r *http.Request, client *http.Client, host state.HostID, path string) {
	request, err := http.NewRequestWithContext(r.Context(), http.MethodGet,
		"https://"+s.config.APIAddress(host)+path, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	request.Header.Set(VersionHeader, s.config.Version)
	response, err := client.Do(request)
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Errorf("ask host %d: %w", host, err))
		return
	}
	defer func() { _ = response.Body.Close() }() // copied to the end below
	w.Header().Set("Content-Type", response.Header.Get("Content-Type"))
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(flushingWriter{w}, response.Body) // the caller is gone if this fails
}
