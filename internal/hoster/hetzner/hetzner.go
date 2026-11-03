// Package hetzner is the Hetzner Cloud API as clowder uses it: Floating IPs
// and the power of servers. It is a small client of its own: the official
// SDK would bring Prometheus' client and much more into clowderd.
package hetzner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
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
	// firstWait and lastWait bound the wait before a request is sent
	// again (do); it doubles from the first to the last.
	firstWait, lastWait time.Duration
}

// attempts is how often do sends a request at most.
const attempts = 5

// New is a client of the API at api (DefaultAPI when empty) with the
// project's token.
func New(api, token string) *Client {
	if api == "" {
		api = DefaultAPI
	}
	return &Client{
		api:       strings.TrimSuffix(api, "/"),
		token:     token,
		http:      &http.Client{Timeout: 30 * time.Second},
		poll:      time.Second,
		firstWait: time.Second,
		lastWait:  time.Minute,
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

// PowerOff cuts the server's power, as pulling the plug would, and waits
// until Hetzner has done it. A server that is off stays off.
func (c *Client) PowerOff(ctx context.Context, serverRef string) error {
	return c.power(ctx, serverRef, "poweroff", "power server %s off")
}

// PowerOn switches the server on and waits until Hetzner has done it. A
// server that runs keeps running.
func (c *Client) PowerOn(ctx context.Context, serverRef string) error {
	return c.power(ctx, serverRef, "poweron", "power server %s on")
}

// power runs a power action on the server; what names it in the error.
func (c *Client) power(ctx context.Context, serverRef, action, what string) error {
	server, err := serverID(serverRef)
	if err != nil {
		return err
	}
	return c.act(ctx, fmt.Sprintf("/servers/%d/actions/%s", server, action), nil, fmt.Sprintf(what, serverRef))
}

// ServerAddresses are the server's IPv4 addresses: in the project's
// private networks, and its public one.
func (c *Client) ServerAddresses(ctx context.Context, serverRef string) ([]netip.Addr, error) {
	server, err := serverID(serverRef)
	if err != nil {
		return nil, err
	}
	var answer struct {
		Server struct {
			PublicNet struct {
				IPv4 *struct {
					IP string `json:"ip"`
				} `json:"ipv4"`
			} `json:"public_net"`
			PrivateNet []struct {
				IP string `json:"ip"`
			} `json:"private_net"`
		} `json:"server"`
	}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/servers/%d", server), nil, &answer); err != nil {
		if errors.Is(err, errNotFound) {
			return nil, fmt.Errorf("read server %s: %w", serverRef, ErrNoSuchServer)
		}
		return nil, fmt.Errorf("read server %s: %w", serverRef, err)
	}
	var texts []string
	for _, network := range answer.Server.PrivateNet {
		texts = append(texts, network.IP)
	}
	if public := answer.Server.PublicNet.IPv4; public != nil {
		texts = append(texts, public.IP)
	}
	var addresses []netip.Addr
	for _, text := range texts {
		address, err := netip.ParseAddr(text)
		if err != nil {
			return nil, fmt.Errorf("server %s: address %q: %w", serverRef, text, err)
		}
		addresses = append(addresses, address)
	}
	return addresses, nil
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

// do sends a request with the token and reads the answer into out. It
// sends it again, up to attempts times, after an error that may pass
// (apiError.passes, or the network): the leader fences through this, and
// one busy moment of the API must not hold up a failover. Every request
// clowder sends may go twice: reads, and actions that change nothing the
// second time (an assignment to the same server, a power action on a
// server in that state). An error carries the API's message, never the
// token.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var encoded []byte
	if body != nil {
		var err error
		if encoded, err = json.Marshal(body); err != nil {
			return err
		}
	}
	wait := c.firstWait
	for attempt := 1; ; attempt++ {
		err := c.send(ctx, method, path, encoded, out)
		var failure *apiError
		isAPIError := errors.As(err, &failure)
		switch {
		case err == nil:
			return nil
		case attempt == attempts || ctx.Err() != nil:
			return err
		case isAPIError && !failure.passes():
			return err
		}
		pause := jitter(wait)
		if isAPIError && failure.reset > pause {
			pause = min(failure.reset, c.lastWait) // the rate limit has room again then
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w (and %w)", err, ctx.Err())
		case <-time.After(pause):
		}
		wait = min(2*wait, c.lastWait)
	}
}

// jitter is a wait between half of it and all of it, so that the leader's
// loops do not ask again in step.
func jitter(wait time.Duration) time.Duration {
	return wait/2 + rand.N(wait/2+1) //nolint:gosec // a wait, no secret
}

// send sends one request; encoded is the body, nil for none.
func (c *Client) send(ctx context.Context, method, path string, encoded []byte, out any) error {
	var reader io.Reader
	if encoded != nil {
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.api+path, reader)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	if encoded != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return err // the network, which may pass
	}
	defer func() { _ = response.Body.Close() }() // read only
	if response.StatusCode >= http.StatusMultipleChoices {
		return readError(response)
	}
	if err := json.NewDecoder(response.Body).Decode(out); err != nil {
		return fmt.Errorf("read the answer: %w", err)
	}
	return nil
}

// apiError is an answer of the API that is not 2xx.
type apiError struct {
	status  statusCode
	code    string // the API's own, such as "locked"
	message string
	// reset is how long until the rate limit has room again
	reset time.Duration
}

func (e *apiError) Error() string {
	if e.message == "" {
		return e.status.Error()
	}
	return fmt.Sprintf("%s (%s, %s)", e.message, e.code, e.status)
}

func (e *apiError) Unwrap() error { return e.status }

// passes tells whether the same request may succeed a little later: the
// project's requests are used up (429), the API has trouble (5xx), or
// another action runs on the same server (locked).
func (e *apiError) passes() bool {
	return e.status == http.StatusTooManyRequests || e.status >= http.StatusInternalServerError ||
		e.status == http.StatusLocked || e.code == "locked"
}

// readError reads the API's error. RateLimit-Reset is when the rate limit
// is full again, as a Unix time; the API sends no Retry-After.
func readError(response *http.Response) *apiError {
	var failure struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&failure) // the status says enough without it
	answer := &apiError{status: statusCode(response.StatusCode), code: failure.Error.Code, message: failure.Error.Message}
	if reset, err := strconv.ParseInt(response.Header.Get("RateLimit-Reset"), 10, 64); err == nil {
		answer.reset = max(0, time.Until(time.Unix(reset, 0)))
	}
	return answer
}

// statusCode is the HTTP status of an answer that is not 2xx.
type statusCode int

func (s statusCode) Error() string { return fmt.Sprintf("HTTP %d", int(s)) }

// errNotFound is the API's 404.
var errNotFound error = statusCode(http.StatusNotFound)

// ErrNoSuchServer is ServerAddresses' answer for a server the project does
// not have, or a server ref that is no server ID.
var ErrNoSuchServer = errors.New("the project has no such server")

// serverID reads a ServerRef as Hetzner's server ID.
func serverID(serverRef string) (int64, error) {
	id, err := strconv.ParseInt(serverRef, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("server ref %q is not a Hetzner server ID: %w", serverRef, ErrNoSuchServer)
	}
	return id, nil
}
