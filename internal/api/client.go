package api

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/webcodingcat/clowder/internal/pki"
)

// Client is the CLI's connection to the cluster. It tries the endpoints in
// turn until one answers, and it learns no other agents. After a write, it
// asks later reads to wait for that write, whichever agent answers them.
type Client struct {
	endpoints []string
	// http has a timeout; streaming has none (for logs that follow)
	http      *http.Client
	streaming *http.Client
	version   string
	written   uint64
	// answered is the endpoint that answered last; the next request tries
	// it first. It is atomic because a watch runs beside other requests.
	answered atomic.Pointer[string]
}

// NewClient connects to the agents at endpoints with a certificate: the
// CLI's, or a host's for clowderd's own commands. It accepts only
// control-plane hosts; a worker among the endpoints fails the handshake and
// is skipped like an endpoint that cannot be reached.
func NewClient(endpoints []string, connectTimeout time.Duration, certificate tls.Certificate, ca *x509.Certificate,
	version string,
) *Client {
	transport := endpointTransport(pki.ClientTLSToControlPlane(certificate, ca, nil), connectTimeout)
	return &Client{
		endpoints: endpoints,
		version:   version,
		http:      &http.Client{Timeout: answerTimeout, Transport: transport},
		streaming: &http.Client{Transport: transport},
	}
}

// answerTimeout is how long the CLI waits for a request, including its
// connection.
const answerTimeout = 30 * time.Second

// DefaultConnectTimeout is how long the CLI waits for an endpoint to
// connect, and again for TLS: long enough for a TLS handshake across the
// internet, short enough that a host that is cut off does not hold up the
// next endpoint for long.
const DefaultConnectTimeout = 5 * time.Second

// endpointTransport is the transport of every client that asks endpoints.
// A host that is cut off drops the packets, so without connectTimeout the
// request's own timeout would pass before the next endpoint is tried.
func endpointTransport(tlsConfig *tls.Config, connectTimeout time.Duration) *http.Transport {
	return &http.Transport{
		TLSClientConfig:     tlsConfig,
		DialContext:         (&net.Dialer{Timeout: connectTimeout}).DialContext,
		TLSHandshakeTimeout: connectTimeout,
	}
}

// Get reads path into out.
func (c *Client) Get(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodGet, path, nil, out)
}

// Post sends in (nil for no body) to path. Later reads wait for the write.
func (c *Client) Post(ctx context.Context, path string, in any) (Written, error) {
	return c.sendChange(ctx, http.MethodPost, path, in)
}

// Act posts in (nil for no body) to path and reads the answer into out,
// for an action whose answer is more than the index of a write.
func (c *Client) Act(ctx context.Context, path string, in, out any) error {
	var body []byte
	if in != nil {
		encoded, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("encode the request: %w", err)
		}
		body = encoded
	}
	return c.do(ctx, http.MethodPost, path, body, out)
}

// ApplyResources puts resources in order and returns the outcome for each.
// A dry run commits nothing. Later reads wait for what it committed.
func (c *Client) ApplyResources(ctx context.Context, request ApplyRequest, dryRun bool) (ChangeResult, error) {
	return c.change(ctx, "/v1/apply", request, dryRun)
}

// DeleteResources deletes resources in order and returns the outcome for
// each. A dry run commits nothing. Later reads wait for what it committed.
func (c *Client) DeleteResources(ctx context.Context, request DeleteRequest, dryRun bool) (ChangeResult, error) {
	return c.change(ctx, "/v1/delete", request, dryRun)
}

func (c *Client) change(ctx context.Context, path string, request any, dryRun bool) (ChangeResult, error) {
	encoded, err := json.Marshal(request)
	if err != nil {
		return ChangeResult{}, fmt.Errorf("encode the request: %w", err)
	}
	if dryRun {
		path += "?dryRun=true"
	}
	var result ChangeResult
	if err := c.do(ctx, http.MethodPost, path, encoded, &result); err != nil {
		return result, err
	}
	c.written = max(c.written, result.Index)
	return result, nil
}

// Stream copies the answer for path to out as it arrives (for logs that
// follow). It has no timeout of its own; only ctx ends it.
func (c *Client) Stream(ctx context.Context, path string, out io.Writer) error {
	body, err := c.openStream(ctx, path)
	if err != nil {
		return err
	}
	defer func() { _ = body.Close() }() // copied to the end below
	_, err = io.Copy(out, body)
	if ctx.Err() != nil {
		return nil // the caller stopped following
	}
	if err != nil {
		return fmt.Errorf("copy the stream: %w", err)
	}
	return nil
}

// Watch passes each event of a watch (GET /v1/watch) to each. It returns
// nil when ctx ends, and io.EOF when the agent ends the stream; the caller
// then lists again.
func (c *Client) Watch(ctx context.Context, path string, each func(event WatchEvent) error) error {
	body, err := c.openStream(ctx, path)
	if err != nil {
		return err
	}
	defer func() { _ = body.Close() }() // read to the end below
	decoder := json.NewDecoder(body)
	for {
		var event WatchEvent
		err := decoder.Decode(&event)
		switch {
		case ctx.Err() != nil:
			return nil // the caller stopped watching
		case errors.Is(err, io.EOF):
			return io.EOF
		case err != nil:
			return fmt.Errorf("read the watch: %w", err)
		}
		if err := each(event); err != nil {
			return err
		}
	}
}

// openStream has no timeout: the answer streams until ctx ends.
func (c *Client) openStream(ctx context.Context, path string) (io.ReadCloser, error) {
	var unreachable []string
	for _, endpoint := range c.inTurn() {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+Address(endpoint)+path, nil)
		if err != nil {
			return nil, fmt.Errorf("ask %s: %w", endpoint, err)
		}
		request.Header.Set(VersionHeader, c.version)
		response, err := c.streaming.Do(request)
		if isForeignCA(err) {
			return nil, foreignCA(endpoint, err)
		}
		if err != nil {
			unreachable = append(unreachable, fmt.Sprintf("%s: %v", endpoint, err))
			continue
		}
		c.answered.Store(&endpoint)
		if response.StatusCode >= 300 {
			defer func() { _ = response.Body.Close() }() // read for the message
			return nil, statusError(response)
		}
		return response.Body, nil
	}
	return nil, fmt.Errorf("no agent answered:\n  %s", strings.Join(unreachable, "\n  "))
}

// inTurn puts the endpoint that answered last first, so an endpoint that
// is down costs its connect timeout once per command, not once per request.
func (c *Client) inTurn() []string {
	last := c.answered.Load()
	if last == nil {
		return c.endpoints
	}
	index := slices.Index(c.endpoints, *last)
	if index <= 0 {
		return c.endpoints
	}
	return append(slices.Clone(c.endpoints[index:]), c.endpoints[:index]...)
}

// sendChange remembers the write's index, so that later reads wait for it.
func (c *Client) sendChange(ctx context.Context, method, path string, in any) (Written, error) {
	var body []byte
	if in != nil {
		encoded, err := json.Marshal(in)
		if err != nil {
			return Written{}, fmt.Errorf("encode the request: %w", err)
		}
		body = encoded
	}
	var written Written
	if err := c.do(ctx, method, path, body, &written); err != nil {
		return written, err
	}
	c.written = max(c.written, written.Index)
	return written, nil
}

// do skips an endpoint that cannot be reached. Any answer, even an error,
// ends the search.
func (c *Client) do(ctx context.Context, method, path string, body []byte, out any) error {
	var unreachable []string
	for _, endpoint := range c.inTurn() {
		request, err := http.NewRequestWithContext(ctx, method, "https://"+Address(endpoint)+path, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("ask %s: %w", endpoint, err)
		}
		request.Header.Set(VersionHeader, c.version)
		if method == http.MethodGet && c.written > 0 {
			request.Header.Set(MinIndexHeader, strconv.FormatUint(c.written, 10))
		}
		// try a write on the next endpoint only if it was never sent; a
		// sent write may have committed even though no answer came back
		var sent atomic.Bool // set in the transport's goroutine
		request = request.WithContext(httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
			WroteRequest: func(httptrace.WroteRequestInfo) { sent.Store(true) },
		}))
		err = send(c.http, request, out)
		var statusError *StatusError
		switch {
		case err == nil || errors.As(err, &statusError):
			// any answer, even an error, shows that the endpoint is
			// reachable, which is all that inTurn cares about
			c.answered.Store(&endpoint)
			return err
		case isForeignCA(err):
			return foreignCA(endpoint, err)
		case sent.Load() && method != http.MethodGet:
			return fmt.Errorf("%s: %w (the write may have gone through: look before you repeat it)", endpoint, err)
		}
		unreachable = append(unreachable, fmt.Sprintf("%s: %v", endpoint, err))
	}
	return fmt.Errorf("no agent answered:\n  %s", strings.Join(unreachable, "\n  "))
}

func isForeignCA(err error) bool {
	var unknown x509.UnknownAuthorityError
	return errors.As(err, &unknown)
}

// foreignCA explains what a foreign CA most likely means, instead of
// showing only x509's message. The next endpoint is not tried, because the
// context's endpoints all belong to the same cluster.
func foreignCA(endpoint string, err error) error {
	return fmt.Errorf("%s belongs to another cluster than your context: another cluster at that address, "+
		"or one set up anew? (%w)", endpoint, err)
}

// statusError falls back to the body's plain text when it holds no Error.
func statusError(response *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20)) // for the message only
	var answer Error
	if json.Unmarshal(body, &answer) != nil || answer.Message == "" {
		answer.Message = strings.TrimSpace(string(body))
	}
	return &StatusError{Status: response.StatusCode, Message: answer.Message}
}

// send turns an answer other than 2xx into a StatusError.
func send(client *http.Client, request *http.Request, out any) error {
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }() // read to the end below
	if response.StatusCode >= 300 {
		return statusError(response)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return fmt.Errorf("read the answer: %w", err)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("read the answer: %w", err)
	}
	return nil
}
