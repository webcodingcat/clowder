// Package docker is the agent's way to Docker: a small client of Docker's
// API on its Unix socket (events, start, restart, list) and docker compose
// for the stacks, run through internal/command like every host tool.
package docker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Socket is where Docker's API listens.
const Socket = "/var/run/docker.sock"

// requestTimeout bounds a call to Docker's API (not the event stream).
const requestTimeout = 30 * time.Second

// Client talks to Docker's API on its Unix socket.
type Client struct {
	http *http.Client
}

// NewClient connects to the socket at path.
func NewClient(path string) *Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", path)
		},
	}
	return &Client{http: &http.Client{Transport: transport}}
}

// Event is what Docker reports about a container.
type Event struct {
	// Action is "die", "start", "stop", "health_status: unhealthy", ...
	Action      string
	ContainerID string
	// Labels are the container's labels (clowder.stack, Compose's service).
	Labels   map[string]string
	ExitCode int
	// Signal is what a kill sent (15 for docker stop, 1 for a reload, ...).
	Signal int
}

// Events streams the events of containers with a label until ctx ends or
// the stream breaks; the error channel then gets why.
func (c *Client) Events(ctx context.Context, label string) (<-chan Event, <-chan error) {
	events := make(chan Event)
	failed := make(chan error, 1)
	go func() {
		defer close(events)
		filters, _ := json.Marshal(map[string][]string{"type": {"container"}, "label": {label}}) // cannot fail
		request, err := http.NewRequestWithContext(ctx, http.MethodGet,
			"http://docker/events?filters="+url.QueryEscape(string(filters)), nil)
		if err != nil {
			failed <- err
			return
		}
		response, err := c.http.Do(request)
		if err != nil {
			failed <- fmt.Errorf("follow Docker's events: %w", err)
			return
		}
		defer func() { _ = response.Body.Close() }() // a stream: nothing to read after
		decoder := json.NewDecoder(bufio.NewReader(response.Body))
		for {
			var raw struct {
				Action string `json:"Action"`
				Actor  struct {
					ID         string            `json:"ID"`
					Attributes map[string]string `json:"Attributes"`
				} `json:"Actor"`
			}
			if err := decoder.Decode(&raw); err != nil {
				failed <- fmt.Errorf("read Docker's events: %w", err)
				return
			}
			exitCode, _ := strconv.Atoi(raw.Actor.Attributes["exitCode"]) // only die carries one
			signal, _ := strconv.Atoi(raw.Actor.Attributes["signal"])     // only kill carries one
			event := Event{
				Action: raw.Action, ContainerID: raw.Actor.ID, Labels: raw.Actor.Attributes,
				ExitCode: exitCode, Signal: signal,
			}
			select {
			case events <- event:
			case <-ctx.Done():
				return
			}
		}
	}()
	return events, failed
}

// ErrNoContainer is Docker's answer for a container that is gone.
var ErrNoContainer = errors.New("no such container")

// Start starts a stopped container.
func (c *Client) Start(ctx context.Context, id string) error {
	return c.post(ctx, "/containers/"+url.PathEscape(id)+"/start")
}

// Restart stops a container and starts it again.
func (c *Client) Restart(ctx context.Context, id string) error {
	return c.post(ctx, "/containers/"+url.PathEscape(id)+"/restart")
}

// Container is a container as the list shows it.
type Container struct {
	ID     string            `json:"Id"`
	State  string            `json:"State"`
	Labels map[string]string `json:"Labels"`
}

// Containers lists all containers (running or not) with a label.
func (c *Client) Containers(ctx context.Context, label string) ([]Container, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	filters, _ := json.Marshal(map[string][]string{"label": {label}}) // cannot fail
	request, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://docker/containers/json?all=1&filters="+url.QueryEscape(string(filters)), nil)
	if err != nil {
		return nil, err
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("list the containers: %w", err)
	}
	defer func() { _ = response.Body.Close() }() // read to the end below
	if response.StatusCode != http.StatusOK {
		return nil, answerError("list the containers", response)
	}
	var containers []Container
	if err := json.NewDecoder(response.Body).Decode(&containers); err != nil {
		return nil, fmt.Errorf("list the containers: %w", err)
	}
	return containers, nil
}

func (c *Client) post(ctx context.Context, path string) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://docker"+path, nil)
	if err != nil {
		return err
	}
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("docker %s: %w", path, err)
	}
	defer func() { _ = response.Body.Close() }() // read to the end below
	if response.StatusCode == http.StatusNotFound {
		return fmt.Errorf("docker %s: %w", path, ErrNoContainer)
	}
	// 304: already in that state
	if response.StatusCode >= 300 && response.StatusCode != http.StatusNotModified {
		return answerError("docker "+path, response)
	}
	return nil
}

func answerError(what string, response *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(response.Body, 4096)) // for the message only
	return fmt.Errorf("%s: HTTP %d: %s", what, response.StatusCode, body)
}
