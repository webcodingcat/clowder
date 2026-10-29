// Package hetzner is the Hetzner Cloud API as clowder uses it: Floating IPs
// and the power of servers. It is a small client of its own: the official
// SDK would bring Prometheus' client and much more into clowderd.
package hetzner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

// DefaultAPI is Hetzner Cloud's API; a test lab has another one.
const DefaultAPI = "https://api.hetzner.cloud/v1"

// Client talks to the API of one Hetzner Cloud project. A server is named
// by its ID, which clowder keeps as the host's ServerRef.
type Client struct {
	api   string
	token string
	http  *http.Client
	// poll is how often it asks whether an action has finished.
	poll time.Duration
}

// New is a client of the API at api (DefaultAPI when empty) with the
// project's token.
func New(api, token string) *Client {
	if api == "" {
		api = DefaultAPI
	}
	return &Client{
		api:   strings.TrimSuffix(api, "/"),
		token: token,
		http:  &http.Client{Timeout: 30 * time.Second},
		poll:  time.Second,
	}
}

// floatingIP is the part of the API's Floating IP that clowder reads.
type floatingIP struct {
	ID     int64  `json:"id"`
	IP     string `json:"ip"`
	Server *int64 `json:"server"`
}

// FloatingIPTarget is the ID of the server the Floating IP is routed to,
// "" for none.
func (c *Client) FloatingIPTarget(ctx context.Context, ip netip.Addr) (string, error) {
	found, err := c.findFloatingIP(ctx, ip)
	if err != nil {
		return "", err
	}
	if found.Server == nil {
		return "", nil
	}
	return strconv.FormatInt(*found.Server, 10), nil
}

// AssignFloatingIP routes the Floating IP to the server and waits until
// Hetzner has done it.
func (c *Client) AssignFloatingIP(ctx context.Context, ip netip.Addr, serverRef string) error {
	server, err := serverID(serverRef)
	if err != nil {
		return err
	}
	found, err := c.findFloatingIP(ctx, ip)
	if err != nil {
		return err
	}
	return c.act(ctx, fmt.Sprintf("/floating_ips/%d/actions/assign", found.ID), map[string]int64{"server": server},
		fmt.Sprintf("assign the Floating IP %s to server %s", ip, serverRef))
}

// PowerOff cuts the server's power and waits until it is off.
func (c *Client) PowerOff(ctx context.Context, serverRef string) error {
	server, err := serverID(serverRef)
	if err != nil {
		return err
	}
	return c.act(ctx, fmt.Sprintf("/servers/%d/actions/poweroff", server), nil, "power off server "+serverRef)
}

// PowerOn starts the server and waits until it runs.
func (c *Client) PowerOn(ctx context.Context, serverRef string) error {
	server, err := serverID(serverRef)
	if err != nil {
		return err
	}
	return c.act(ctx, fmt.Sprintf("/servers/%d/actions/poweron", server), nil, "power on server "+serverRef)
}

// findFloatingIP looks the address up among the project's Floating IPs:
// the API can list them but not find one by its address.
func (c *Client) findFloatingIP(ctx context.Context, ip netip.Addr) (floatingIP, error) {
	for page := 1; page != 0; {
		var answer struct {
			FloatingIPs []floatingIP `json:"floating_ips"`
			Meta        struct {
				Pagination struct {
					NextPage *int `json:"next_page"`
				} `json:"pagination"`
			} `json:"meta"`
		}
		path := "/floating_ips?per_page=50&page=" + strconv.Itoa(page)
		if err := c.do(ctx, http.MethodGet, path, nil, &answer); err != nil {
			return floatingIP{}, fmt.Errorf("list the Floating IPs: %w", err)
		}
		for _, candidate := range answer.FloatingIPs {
			// an IPv6 Floating IP is a network, which never parses as an address
			if address, err := netip.ParseAddr(candidate.IP); err == nil && address == ip {
				return candidate, nil
			}
		}
		page = 0
		if next := answer.Meta.Pagination.NextPage; next != nil {
			page = *next
		}
	}
	return floatingIP{}, fmt.Errorf("the project has no Floating IP %s", ip)
}

// action is an asynchronous change the API reports on.
type action struct {
	ID     int64  `json:"id"`
	Status string `json:"status"` // running, success or error
	Error  *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// act starts an action and waits until it has finished; what says what was
// tried, for the error.
func (c *Client) act(ctx context.Context, path string, body any, what string) error {
	var answer struct {
		Action action `json:"action"`
	}
	if err := c.do(ctx, http.MethodPost, path, body, &answer); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	for {
		switch current := answer.Action; current.Status {
		case "success":
			return nil
		case "error":
			if current.Error != nil {
				return fmt.Errorf("%s: %s (%s)", what, current.Error.Message, current.Error.Code)
			}
			return fmt.Errorf("%s: the action failed", what)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s: %w", what, ctx.Err())
		case <-time.After(c.poll):
		}
		if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/actions/%d", answer.Action.ID), nil, &answer); err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
	}
}

// do sends one request with the token and reads the answer into out. An
// error carries the API's message, never the token.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.api+path, reader)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }() // read only
	if response.StatusCode >= http.StatusMultipleChoices {
		var failure struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&failure) // the status says enough without it
		if failure.Error.Message != "" {
			return fmt.Errorf("%s (%s, HTTP %d)", failure.Error.Message, failure.Error.Code, response.StatusCode)
		}
		return fmt.Errorf("HTTP %d", response.StatusCode)
	}
	if err := json.NewDecoder(response.Body).Decode(out); err != nil {
		return fmt.Errorf("read the answer: %w", err)
	}
	return nil
}

// serverID reads a ServerRef as Hetzner's server ID.
func serverID(serverRef string) (int64, error) {
	id, err := strconv.ParseInt(serverRef, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("server ref %q is not a Hetzner server ID", serverRef)
	}
	return id, nil
}
