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

	"github.com/webcodingcat/clowder/internal/clusterdir"
	"github.com/webcodingcat/clowder/internal/compose"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/state"
	"github.com/webcodingcat/clowder/internal/units"
)

// StackApply is clowder stack apply: the stack's Compose file as
// compose.Stack.Source writes it, and the values of the secrets it asks for
// (name -> value). The values travel in clear inside mutual TLS, like the
// cluster's secrets; the agent seals them before they go into the state.
type StackApply struct {
	Source  string            `json:"source"`
	Secrets map[string]string `json:"secrets,omitempty"`
}

// StackInfo is one stack as the CLI shows it.
type StackInfo struct {
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
	Owner      string `json:"owner"`
	CreatedBy  string `json:"created_by,omitempty"`
	Generation int64  `json:"generation"`
	Priority   int    `json:"priority"`
	Status     string `json:"status"`
	Host       string `json:"host,omitempty"` // empty: not placed yet
	// PushedOut are the services stopped for others of higher priority.
	PushedOut []string `json:"pushed_out,omitempty"`
	// WaitReason is why the stack or a volume it newly names finds no place.
	WaitReason string `json:"wait_reason,omitempty"`
}

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

// stackNamespace is the namespace of the stacks the stack routes name;
// clowder apply brings the others (phase 1c).
const stackNamespace = names.Default

// createStack registers a stack for clowder stack apply. The same user
// creating a stack again that was never applied does nothing, so a create
// whose files could not be written can be repeated.
func (s *Server) createStack(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	caller := identityOf(r).Name
	var registered bool
	s.config.Cluster.Read(func(current *state.State) {
		stack, ok := current.Stacks[names.ID(stackNamespace, name)]
		registered = ok && stack.Status == state.StackCreated && stack.Owner == state.OwnerCLI &&
			stack.CreatedBy == caller
	})
	if registered {
		writeJSON(w, http.StatusOK, Written{})
		return
	}
	s.write(w, r, state.KindCreateStack, state.CreateStack{Namespace: stackNamespace, Name: name, Owner: state.OwnerCLI})
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
	var revision int64
	var changedVolumes error
	s.config.Cluster.Read(func(current *state.State) {
		config, revision = current.Config.Spec, current.Config.Revision
		changedVolumes = checkVolumesUnchanged(stackNamespace, stack, current.Volumes)
	})
	if changedVolumes != nil {
		writeError(w, http.StatusBadRequest, changedVolumes)
		return
	}
	priority, err := checkAgainstCluster(stackNamespace, stack, config)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	secrets, err := s.sealStackSecrets(stack, apply.Secrets)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.write(w, r, state.KindPutStack, state.PutStack{
		Namespace: stackNamespace, Name: name, Owner: state.OwnerCLI, Source: apply.Source, Priority: priority,
		Secrets: secrets, Ports: stack.PublishedPorts(), Routes: stack.Routes(), Services: services(stack),
		Privileged: privileged(stack), ConfigRevision: revision,
	})
}

// privileged tells whether a service of the stack reaches beyond its
// container.
func privileged(stack *compose.Stack) bool {
	for _, service := range stack.Services {
		if len(service.Privileges) > 0 {
			return true
		}
	}
	return false
}

// sealStackSecrets checks that the values sent are exactly those the stack
// asks for, within the limits, and seals them. It is the one way stack
// secrets come in; a later one (the cluster decrypting a file for GitOps)
// goes next to it. No message names a value.
func (s *Server) sealStackSecrets(stack *compose.Stack, values map[string]string) (map[string]state.Sealed, error) {
	asked := stack.SecretNames()
	sent := slices.Sorted(maps.Keys(values))
	switch {
	case !slices.Equal(asked, sent):
		return nil, fmt.Errorf("stack %s asks for the secrets %v, but %v came", stack.Name, asked, sent)
	case len(asked) > state.MaxStackSecrets:
		return nil, fmt.Errorf("stack %s asks for %d secrets, more than %d", stack.Name, len(asked), state.MaxStackSecrets)
	}
	sealed := map[string]state.Sealed{}
	total := 0
	for _, name := range asked {
		value := values[name]
		total += len(value)
		switch {
		case value == "":
			return nil, fmt.Errorf("secret %s is empty", name)
		case len(value) > state.MaxSecretBytes:
			return nil, fmt.Errorf("secret %s is larger than %d bytes", name, state.MaxSecretBytes)
		case total > state.MaxStackSecretsBytes:
			return nil, fmt.Errorf("the secrets of stack %s are larger than %d bytes", stack.Name, state.MaxStackSecretsBytes)
		}
		var err error
		if sealed[name], err = s.config.Keys.Seal([]byte(value)); err != nil {
			return nil, fmt.Errorf("seal secret %s: %w", name, err)
		}
	}
	if len(sealed) == 0 {
		return nil, nil
	}
	return sealed, nil
}

// checkVolumesUnchanged refuses another class, size or number of replicas
// for a volume the stack has already: nothing changes an existing volume
// yet, so such an apply would seem to work and do nothing.
func checkVolumesUnchanged(namespace string, stack *compose.Stack, volumes map[string]*state.Volume) error {
	var problems compose.Problems
	for _, name := range slices.Sorted(maps.Keys(stack.Volumes)) {
		existing, ok := volumes[names.VolumeID(namespace, stack.Name, name)]
		if !ok {
			continue
		}
		wanted := stack.Volumes[name]
		if wanted.Class != existing.Class || wanted.Size != existing.SizeBytes || wanted.Replicas != existing.Replicas {
			problems = append(problems, fmt.Sprintf(
				"volume %s exists with class %s, size %s and %d replicas; clowder cannot change a volume yet",
				name, existing.Class, units.FormatSize(existing.SizeBytes), existing.Replicas))
		}
	}
	if len(problems) > 0 {
		return problems
	}
	return nil
}

// checkAgainstCluster checks what only the cluster knows (storage classes,
// priority categories, privileges) and returns the stack's priority as a
// number. The state checks the privileges again, at commit.
func checkAgainstCluster(namespace string, stack *compose.Stack, config clusterdir.Config) (int, error) {
	var problems compose.Problems
	for _, name := range slices.Sorted(maps.Keys(stack.Volumes)) {
		if _, ok := config.StorageClasses[stack.Volumes[name].Class]; !ok {
			problems = append(problems, fmt.Sprintf("volume %s: the cluster has no storage class %q",
				name, stack.Volumes[name].Class))
		}
	}
	priority, err := config.Priorities.Resolve(stack.Priority)
	if err != nil {
		problems = append(problems, "stack: "+err.Error())
	}
	privileged := slices.Contains(config.Stacks.Privileged, names.ID(namespace, stack.Name))
	for _, name := range slices.Sorted(maps.Keys(stack.Services)) {
		service := stack.Services[name]
		if _, err := config.Priorities.Resolve(service.Priority); err != nil {
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

// services are what the scheduler needs of the stack's services: the
// priority as written (the service's own, else the stack's), the
// reservations (zero: the cluster's default) and what each depends on.
func services(stack *compose.Stack) map[string]state.Service {
	services := map[string]state.Service{}
	for name, service := range stack.Services {
		priority := service.Priority
		if priority == (clusterdir.Priority{}) {
			priority = stack.Priority
		}
		services[name] = state.Service{
			Priority: priority, MemoryBytes: service.MemoryReservation, CPUs: service.CPUReservation,
			DependsOn: service.DependsOn,
		}
	}
	return services
}

func (s *Server) stacks(w http.ResponseWriter, _ *http.Request) {
	stacks := []StackInfo{}
	s.config.Cluster.Read(func(current *state.State) {
		for _, id := range slices.Sorted(maps.Keys(current.Stacks)) {
			stacks = append(stacks, stackInfo(current, current.Stacks[id]))
		}
	})
	writeJSON(w, http.StatusOK, stacks)
}

func stackInfo(current *state.State, stack *state.Stack) StackInfo {
	info := StackInfo{
		Namespace: stack.Namespace, Name: stack.Name, Owner: stack.Owner, CreatedBy: stack.CreatedBy,
		Generation: stack.Generation, Priority: stack.Priority, Status: string(stack.Status),
		PushedOut: stack.PushedOut, WaitReason: stack.WaitReason,
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
	id := names.ID(stackNamespace, name)
	var exists bool
	var volumes []string
	s.config.Cluster.Read(func(current *state.State) {
		_, exists = current.Stacks[id]
		for _, volumeID := range slices.Sorted(maps.Keys(current.Volumes)) {
			if stackID, _ := names.StackOfVolume(volumeID); stackID == id {
				volumes = append(volumes, volumeID)
			}
		}
	})
	switch {
	case !exists:
		writeError(w, http.StatusNotFound, fmt.Errorf("no stack %s", id))
		return
	case len(volumes) > 0 && r.URL.Query().Get("volumes") != "delete":
		writeError(w, http.StatusConflict, fmt.Errorf(
			"stack %s has volumes %v; their data goes with the stack only when you say so (--delete-volumes)",
			id, volumes))
		return
	}
	s.write(w, r, state.KindDeleteStack, state.DeleteStack{
		Namespace: stackNamespace, Name: name, WithVolumes: len(volumes) > 0,
	})
}

// stackHost finds the host a stack runs on and its Compose project there;
// ok is false (and the answer written) when there is none.
func (s *Server) stackHost(w http.ResponseWriter, name string) (state.HostID, string, bool) {
	id := names.ID(stackNamespace, name)
	var host *state.HostID
	var exists, created bool
	var project string
	s.config.Cluster.Read(func(current *state.State) {
		if stack, ok := current.Stacks[id]; ok {
			exists, created, host, project = true, stack.Status == state.StackCreated, stack.Host, stack.Project()
		}
	})
	switch {
	case !exists:
		writeError(w, http.StatusNotFound, fmt.Errorf("no stack %s", id))
		return 0, "", false
	case created:
		writeError(w, http.StatusConflict, fmt.Errorf("stack %s is created, but was never applied", id))
		return 0, "", false
	case host == nil:
		writeError(w, http.StatusConflict, fmt.Errorf("stack %s is not placed on a host yet", id))
		return 0, "", false
	}
	return *host, project, true
}

// stackPs lists a stack's containers, asking its host.
func (s *Server) stackPs(w http.ResponseWriter, r *http.Request) {
	host, project, ok := s.stackHost(w, r.PathValue("name"))
	if !ok {
		return
	}
	if host != s.config.HostID {
		s.relay(w, r, s.forwarder, host, "/v1/internal/stacks/"+url.PathEscape(project)+"/ps")
		return
	}
	s.ps(w, r, project)
}

// stackLogs writes a stack's logs, from its host; ?follow=1 keeps going.
func (s *Server) stackLogs(w http.ResponseWriter, r *http.Request) {
	host, project, ok := s.stackHost(w, r.PathValue("name"))
	if !ok {
		return
	}
	if host != s.config.HostID {
		s.relay(w, r, s.streamer, host, "/v1/internal/stacks/"+url.PathEscape(project)+"/logs?"+r.URL.RawQuery)
		return
	}
	s.logs(w, r, project)
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
	follow, _ := strconv.ParseBool(r.URL.Query().Get("follow")) // anything else: no
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	writer := flushingWriter{w}
	if err := s.config.Containers.Logs(r.Context(), project, follow, writer); err != nil {
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
