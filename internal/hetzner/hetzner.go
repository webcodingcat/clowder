// Package hetzner is the Hetzner Cloud API as clowder uses it: for the
// leader it routes the Floating IP to the ingress host, checks which server
// a host runs on, and powers servers off (fencing) and on; clowderd recover
// labels the servers it takes out of the cluster, and an agent reads the
// label when it starts. A server is named by its ID, which the state keeps
// as the host's ServerID. It is a small client of its own: the official SDK
// would bring Prometheus' client and much more into clowderd.
package hetzner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math/rand/v2"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"
)

// DefaultAPI is Hetzner Cloud's API; a test lab has another one.
const DefaultAPI = "https://api.hetzner.cloud/v1"

// Client talks to the API of one Hetzner Cloud project.
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

// maxErrorBody bounds what is read of an answer: an error's body, or the
// rest of one nobody reads.
const maxErrorBody = 64 << 10

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

// FloatingIPTarget is the ID of the server the Floating IP is routed to, 0
// for none.
func (c *Client) FloatingIPTarget(ctx context.Context, ip netip.Addr) (int64, error) {
	found, err := c.findFloatingIP(ctx, ip)
	if err != nil {
		return 0, err
	}
	if found.Server == nil {
		return 0, nil
	}
	return *found.Server, nil
}

// AssignFloatingIP routes the Floating IP to the server and waits until
// Hetzner has done it.
func (c *Client) AssignFloatingIP(ctx context.Context, ip netip.Addr, serverID int64) error {
	if err := checkID(serverID); err != nil {
		return err
	}
	found, err := c.findFloatingIP(ctx, ip)
	if err != nil {
		return err
	}
	return c.act(ctx, fmt.Sprintf("/floating_ips/%d/actions/assign", found.ID), map[string]int64{"server": serverID},
		fmt.Sprintf("assign the Floating IP %s to server %d", ip, serverID))
}

// PowerOff cuts the server's power, as pulling the plug would, and waits
// until Hetzner has done it: fencing relies on it. A server that is off
// stays off.
func (c *Client) PowerOff(ctx context.Context, serverID int64) error {
	return c.power(ctx, serverID, "poweroff", "power server %d off")
}

// PowerOn switches the server on and waits until Hetzner has done it. A
// server that runs keeps running.
func (c *Client) PowerOn(ctx context.Context, serverID int64) error {
	return c.power(ctx, serverID, "poweron", "power server %d on")
}

// power runs a power action on the server; what names it in the error.
func (c *Client) power(ctx context.Context, serverID int64, action, what string) error {
	if err := checkID(serverID); err != nil {
		return err
	}
	return c.act(ctx, fmt.Sprintf("/servers/%d/actions/%s", serverID, action), nil, fmt.Sprintf(what, serverID))
}

// server is the part of the API's server that clowder reads.
type server struct {
	ID        int64 `json:"id"`
	PublicNet struct {
		IPv4 *struct {
			IP string `json:"ip"`
		} `json:"ipv4"`
	} `json:"public_net"`
	PrivateNet []struct {
		IP string `json:"ip"`
	} `json:"private_net"`
	Labels map[string]string `json:"labels"`
}

// readServer reads the server: ErrNoSuchServer for one the project does
// not have.
func (c *Client) readServer(ctx context.Context, serverID int64) (server, error) {
	if err := checkID(serverID); err != nil {
		return server{}, err
	}
	var answer struct {
		Server server `json:"server"`
	}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/servers/%d", serverID), nil, &answer); err != nil {
		if errors.Is(err, errNotFound) {
			return server{}, fmt.Errorf("read server %d: %w", serverID, ErrNoSuchServer)
		}
		return server{}, fmt.Errorf("read server %d: %w", serverID, err)
	}
	return answer.Server, nil
}

// ErrOtherServer is CheckServer's answer for a server that is not the
// host's.
var ErrOtherServer = errors.New("it is another server")

// CheckServer makes sure the server is the host's: the host's underlay is
// one of the server's addresses in the project's private networks, and its
// public address is the server's public IPv4. A host names its server
// itself, and fencing powers off the server its ID names; the project's
// token may power off any server of the project.
func (c *Client) CheckServer(ctx context.Context, serverID int64, underlay, publicAddress netip.Addr) error {
	found, err := c.readServer(ctx, serverID)
	if errors.Is(err, ErrNoSuchServer) {
		// a server the project does not have is no server of the host
		return fmt.Errorf("check server %d: %w: %w", serverID, err, ErrOtherServer)
	}
	if err != nil {
		return fmt.Errorf("check server %d: %w", serverID, err)
	}
	var private []netip.Addr
	for _, network := range found.PrivateNet {
		if address, err := netip.ParseAddr(network.IP); err == nil {
			private = append(private, address)
		}
	}
	var public netip.Addr
	if ipv4 := found.PublicNet.IPv4; ipv4 != nil {
		public, _ = netip.ParseAddr(ipv4.IP) // an address that does not parse is no host's
	}
	switch {
	case !slices.Contains(private, underlay):
		return fmt.Errorf("server %d has the private addresses %v, not the host's underlay %s: %w",
			serverID, private, underlay, ErrOtherServer)
	case public != publicAddress:
		return fmt.Errorf("server %d has the public address %s, not the host's %s: %w",
			serverID, public, publicAddress, ErrOtherServer)
	}
	return nil
}

// RetiredLabel marks a server that clowderd recover took out of its
// cluster. Its value is the serial of the retired host's certificate, so a
// server set up anew, with a certificate of its own, is not retired. The
// agent of a retired host starts nothing: with another retired one it
// would still have the old cluster's majority.
const RetiredLabel = "clowder-retired"

// SetRetired labels the server RetiredLabel with the host's certificate
// serial and keeps its other labels: the API replaces all of them at once.
func (c *Client) SetRetired(ctx context.Context, serverID int64, serial string) error {
	found, err := c.readServer(ctx, serverID)
	if err != nil {
		return err
	}
	if found.Labels[RetiredLabel] == serial {
		return nil
	}
	labels := maps.Clone(found.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	labels[RetiredLabel] = serial
	body := map[string]map[string]string{"labels": labels}
	if err := c.do(ctx, http.MethodPut, fmt.Sprintf("/servers/%d", found.ID), body, nil); err != nil {
		return fmt.Errorf("label server %d %s: %w", serverID, RetiredLabel, err)
	}
	return nil
}

// RetiredSerial is the serial of the host certificate whose host recover
// retired on the server; "" for a server it did not.
func (c *Client) RetiredSerial(ctx context.Context, serverID int64) (string, error) {
	found, err := c.readServer(ctx, serverID)
	if err != nil {
		return "", err
	}
	return found.Labels[RetiredLabel], nil
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
	defer func() {
		// read to the end, so the connection can carry the next request
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxErrorBody))
		_ = response.Body.Close()
	}()
	if response.StatusCode >= http.StatusMultipleChoices {
		return readError(response)
	}
	if out == nil {
		return nil // an answer nobody reads
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
	_ = json.NewDecoder(io.LimitReader(response.Body, maxErrorBody)).Decode(&failure) // the status says enough without it
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

// ErrNoSuchServer is the answer for a server the project does not have, or
// an ID that names no server.
var ErrNoSuchServer = errors.New("the project has no such server")

// checkID refuses an ID no server has; 0 is a host's missing one.
func checkID(serverID int64) error {
	if serverID <= 0 {
		return fmt.Errorf("server ID %d: %w", serverID, ErrNoSuchServer)
	}
	return nil
}
