package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/webcodingcat/clowder/internal/clusterdir"
	"github.com/webcodingcat/clowder/internal/compose"
	"github.com/webcodingcat/clowder/internal/state"
)

// StackApply is clowder stack apply: the stack's Compose file as
// compose.Stack.Source writes it.
type StackApply struct {
	Source string `json:"source"`
}

// StackInfo is one stack as the CLI shows it.
type StackInfo struct {
	Name       string   `json:"name"`
	Owner      string   `json:"owner"`
	CreatedBy  string   `json:"created_by,omitempty"`
	Generation int64    `json:"generation"`
	Priority   int      `json:"priority"`
	Status     string   `json:"status"`
	Host       string   `json:"host,omitempty"` // empty: not placed yet
	Stopped    []string `json:"stopped,omitempty"`
}

// ContainerInfo is one container of a stack on its host.
type ContainerInfo struct {
	Service string `json:"service"`
	State   string `json:"state"`  // running, exited, ...
	Health  string `json:"health"` // healthy, unhealthy, starting, or empty
	Status  string `json:"status"` // Docker's words: "Up 3 minutes"
}

// Containers answers for the stacks that run on this host; the agent is the
// real one.
type Containers interface {
	Ps(ctx context.Context, stack string) ([]ContainerInfo, error)
	// Logs writes the stack's logs to out; with follow, until ctx ends.
	Logs(ctx context.Context, stack string, follow bool, out io.Writer) error
}

// createStack registers a stack for clowder stack apply. Creating a stack
// that is registered and was never applied again does nothing, so a create
// whose files could not be written can be repeated.
func (s *Server) createStack(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var registered bool
	s.config.Cluster.Read(func(current *state.State) {
		stack, ok := current.Stacks[name]
		registered = ok && stack.Status == state.StackCreated && stack.Owner == state.OwnerCLI
	})
	if registered {
		writeJSON(w, http.StatusOK, Written{})
		return
	}
	s.write(w, r, state.KindCreateStack, state.CreateStack{Name: name, Owner: state.OwnerCLI})
}

// applyStack checks a stack against the cluster and commits it.
func (s *Server) applyStack(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var apply StackApply
	if err := json.NewDecoder(r.Body).Decode(&apply); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("read the request: %w", err))
		return
	}
	stack, err := compose.Parse(r.Context(), name, []byte(apply.Source))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var config clusterdir.Config
	s.config.Cluster.Read(func(current *state.State) { config = current.Config.Spec })
	priority, err := checkAgainstCluster(stack, config)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.write(w, r, state.KindPutStack, state.PutStack{
		Name: name, Owner: state.OwnerCLI, Source: apply.Source, Priority: priority,
	})
}

// checkAgainstCluster checks what only the cluster knows (storage classes,
// priority categories) and returns the stack's priority as a number.
func checkAgainstCluster(stack *compose.Stack, config clusterdir.Config) (int, error) {
	var problems compose.Problems
	for _, name := range slices.Sorted(maps.Keys(stack.Volumes)) {
		if _, ok := config.StorageClasses[stack.Volumes[name].Class]; !ok {
			problems = append(problems, fmt.Sprintf("volume %s: the cluster has no storage class %q",
				name, stack.Volumes[name].Class))
		}
	}
	priority, err := PriorityOf(stack.Priority, config.Priorities)
	if err != nil {
		problems = append(problems, "stack: "+err.Error())
	}
	privileged := slices.Contains(config.Stacks.Privileged, stack.Name)
	for _, name := range slices.Sorted(maps.Keys(stack.Services)) {
		service := stack.Services[name]
		if _, err := PriorityOf(service.Priority, config.Priorities); err != nil {
			problems = append(problems, fmt.Sprintf("service %s: %v", name, err))
		}
		for _, privilege := range service.Privileges {
			if !privileged {
				problems = append(problems, fmt.Sprintf(
					"service %s: %s reaches beyond the container; only stacks named in stacks.privileged "+
						"of cluster.yaml may have it", name, privilege))
			}
		}
	}
	if len(problems) > 0 {
		return 0, problems
	}
	return priority, nil
}

// PriorityOf turns a priority of x-clowder into a number with the cluster's
// categories: a number stays, a category is looked up, none is the default.
func PriorityOf(priority compose.Priority, priorities clusterdir.Priorities) (int, error) {
	category := priority.Category
	switch {
	case priority.Value != nil:
		return *priority.Value, nil
	case category == "":
		category = priorities.Default
	}
	value, ok := priorities.Categories[category]
	if !ok {
		return 0, fmt.Errorf("the cluster has no priority category %q", category)
	}
	return value, nil
}

func (s *Server) stacks(w http.ResponseWriter, _ *http.Request) {
	stacks := []StackInfo{}
	s.config.Cluster.Read(func(current *state.State) {
		for _, name := range slices.Sorted(maps.Keys(current.Stacks)) {
			stacks = append(stacks, stackInfo(current, current.Stacks[name]))
		}
	})
	writeJSON(w, http.StatusOK, stacks)
}

func stackInfo(current *state.State, stack *state.Stack) StackInfo {
	info := StackInfo{
		Name: stack.Name, Owner: stack.Owner, CreatedBy: stack.CreatedBy, Generation: stack.Generation,
		Priority: stack.Priority, Status: string(stack.Status), Stopped: stack.Stopped,
	}
	if stack.Host != nil {
		if host, ok := current.Hosts[*stack.Host]; ok {
			info.Host = host.Name
		}
	}
	return info
}

// deleteStack removes a stack. Its volumes go with it, data and all, only
// when the caller says so (?volumes=delete).
func (s *Server) deleteStack(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var exists bool
	var volumes []string
	s.config.Cluster.Read(func(current *state.State) {
		_, exists = current.Stacks[name]
		for _, id := range slices.Sorted(maps.Keys(current.Volumes)) {
			if stackOf(id) == name {
				volumes = append(volumes, id)
			}
		}
	})
	switch {
	case !exists:
		writeError(w, http.StatusNotFound, fmt.Errorf("no stack %s", name))
		return
	case len(volumes) > 0 && r.URL.Query().Get("volumes") != "delete":
		writeError(w, http.StatusConflict, fmt.Errorf(
			"stack %s has volumes %v; their data goes with the stack only when you say so (--delete-volumes)",
			name, volumes))
		return
	}
	s.write(w, r, state.KindDeleteStack, state.DeleteStack{Name: name, WithVolumes: len(volumes) > 0})
}

func stackOf(volumeID string) string {
	stack, _, _ := strings.Cut(volumeID, "/")
	return stack
}

// stackHost finds the host a stack runs on; ok is false (and the answer
// written) when there is none.
func (s *Server) stackHost(w http.ResponseWriter, name string) (state.HostID, bool) {
	var host *state.HostID
	var exists, created bool
	s.config.Cluster.Read(func(current *state.State) {
		if stack, ok := current.Stacks[name]; ok {
			exists, created, host = true, stack.Status == state.StackCreated, stack.Host
		}
	})
	switch {
	case !exists:
		writeError(w, http.StatusNotFound, fmt.Errorf("no stack %s", name))
		return 0, false
	case created:
		writeError(w, http.StatusConflict, fmt.Errorf("stack %s is created, but was never applied", name))
		return 0, false
	case host == nil:
		writeError(w, http.StatusConflict, fmt.Errorf("stack %s is not placed on a host yet", name))
		return 0, false
	}
	return *host, true
}

// stackPs lists a stack's containers, asking its host.
func (s *Server) stackPs(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	host, ok := s.stackHost(w, name)
	if !ok {
		return
	}
	if host != s.config.HostID {
		s.relay(w, r, host, "/v1/internal/stacks/"+url.PathEscape(name)+"/ps")
		return
	}
	s.localPs(w, r)
}

// stackLogs writes a stack's logs, from its host; ?follow=1 keeps going.
func (s *Server) stackLogs(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	host, ok := s.stackHost(w, name)
	if !ok {
		return
	}
	if host != s.config.HostID {
		s.relay(w, r, host, "/v1/internal/stacks/"+url.PathEscape(name)+"/logs?"+r.URL.RawQuery)
		return
	}
	s.localLogs(w, r)
}

func (s *Server) localPs(w http.ResponseWriter, r *http.Request) {
	if s.config.Containers == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("this agent runs no containers"))
		return
	}
	containers, err := s.config.Containers.Ps(r.Context(), r.PathValue("name"))
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err)
		return
	}
	writeJSON(w, http.StatusOK, containers)
}

func (s *Server) localLogs(w http.ResponseWriter, r *http.Request) {
	if s.config.Containers == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("this agent runs no containers"))
		return
	}
	follow, _ := strconv.ParseBool(r.URL.Query().Get("follow")) // anything else: no
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	writer := flushingWriter{w}
	if err := s.config.Containers.Logs(r.Context(), r.PathValue("name"), follow, writer); err != nil {
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

// relay asks another host's internal endpoint and passes its answer on, as
// it streams.
func (s *Server) relay(w http.ResponseWriter, r *http.Request, host state.HostID, path string) {
	request, err := http.NewRequestWithContext(r.Context(), http.MethodGet,
		"https://"+s.config.APIAddress(host)+path, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	request.Header.Set(VersionHeader, s.config.Version)
	response, err := s.streamer.Do(request)
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Errorf("ask host %d: %w", host, err))
		return
	}
	defer func() { _ = response.Body.Close() }() // copied to the end below
	w.Header().Set("Content-Type", response.Header.Get("Content-Type"))
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(flushingWriter{w}, response.Body) // the caller is gone if this fails
}
