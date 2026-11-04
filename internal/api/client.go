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
// turn until one answers (it learns no other agents), and after a write it
// asks reads to wait for that write, whichever agent answers them.
type Client struct {
	endpoints []string
	// http answers within its timeout; streaming has none (logs that follow)
	http      *http.Client
	streaming *http.Client
	version   string
	// written is the index of this client's last write.
	written uint64
	// answered is the endpoint that answered last; the next request tries it
	// first. Atomic: a watch runs beside the other requests.
	answered atomic.Pointer[string]
}

// NewClient connects with a user's certificate to the agents at endpoints.
// An endpoint that has not connected within connectTimeout, or not finished
// TLS within as long again, is skipped like one that refused it: a host cut
// off drops the packets, and without a timeout of its own the whole
// request's would pass before the next endpoint is tried.
func NewClient(endpoints []string, connectTimeout time.Duration, certificate tls.Certificate, ca *x509.Certificate,
	version string,
) *Client {
	transport := endpointTransport(pki.ClientTLSToHosts(certificate, ca, nil), connectTimeout)
	return &Client{
		endpoints: endpoints,
		version:   version,
		http:      &http.Client{Timeout: answerTimeout, Transport: transport},
		streaming: &http.Client{Transport: transport},
	}
}

// answerTimeout is how long the CLI waits for a request, its connection
// included.
const answerTimeout = 30 * time.Second

// DefaultConnectTimeout is how long the CLI waits for an endpoint to
// connect, and again for TLS, unless --connect-timeout says otherwise: long
// enough for a TLS handshake across the internet, short enough that a host
// cut off (whose packets are dropped) does not hold up the next endpoint for
// long.
const DefaultConnectTimeout = 5 * time.Second

// endpointTransport is the transport of every client that asks endpoints:
// the TCP connection and the TLS handshake each have connectTimeout, so an
// endpoint that does not answer fails soon and the next is tried.
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

// Post writes in (nil for none) to path. Reads after it wait for the write.
func (c *Client) Post(ctx context.Context, path string, in any) (Written, error) {
	return c.sendChange(ctx, http.MethodPost, path, in)
}

// Act posts in (nil for none) to path and reads the answer into out: an
// action whose answer is more than the index of a write, such as a test
// alert or a join token.
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

// ApplyResources puts resources in order and reads what became of each;
// a dry run commits nothing. Reads after it wait for what it committed.
func (c *Client) ApplyResources(ctx context.Context, request ApplyRequest, dryRun bool) (ChangeResult, error) {
	return c.change(ctx, "/v1/apply", request, dryRun)
}

// DeleteResources deletes resources in order and reads what became of
// each; a dry run commits nothing. Reads after it wait for what it
// committed.
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

// Rekey has the cluster encrypt values anew for its recipients now
// (POST /v1/encryption/rekey).
func (c *Client) Rekey(ctx context.Context, values []PlacedValue) ([]string, error) {
	encoded, err := json.Marshal(RekeyRequest{Values: values})
	if err != nil {
		return nil, fmt.Errorf("encode the request: %w", err)
	}
	var answer RekeyAnswer
	if err := c.do(ctx, http.MethodPost, "/v1/encryption/rekey", encoded, &answer); err != nil {
		return nil, err
	}
	if len(answer.Values) != len(values) {
		return nil, fmt.Errorf("the cluster answered %d values for %d", len(answer.Values), len(values))
	}
	return answer.Values, nil
}

// DiffCluster asks the cluster what applying cluster.yaml would change,
// with every check of a real apply (PUT /v1/cluster?dryRun=true).
func (c *Client) DiffCluster(ctx context.Context, apply ClusterApply) (ClusterDiff, error) {
	encoded, err := json.Marshal(apply)
	if err != nil {
		return ClusterDiff{}, fmt.Errorf("encode the request: %w", err)
	}
	var diff ClusterDiff
	return diff, c.do(ctx, http.MethodPut, "/v1/cluster?dryRun=true", encoded, &diff)
}

// Put replaces what is at path with in. Reads after it wait for the write.
func (c *Client) Put(ctx context.Context, path string, in any) (Written, error) {
	return c.sendChange(ctx, http.MethodPut, path, in)
}

// Stream copies what path answers to out as it comes (logs that follow);
// it has no timeout of its own, ctx ends it.
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

// Watch hands each event of a watch (GET /v1/watch) to each, until ctx
// ends (nil) or the agent ends the stream (io.EOF: its ring dropped what
// the client had not read yet, so the client lists afresh).
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

// openStream asks the first endpoint that answers for path, without a timeout:
// the answer streams until ctx ends. An answer other than 2xx becomes a
// StatusError.
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

// inTurn is the endpoints in the order to try them: the one that answered
// last first, so that an endpoint that is down costs its connect timeout once per
// command, not once per request.
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

// sendChange sends a request that changes the state, and remembers its
// index so that reads after it wait for it.
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

// do sends a request to the first endpoint that answers. An endpoint that cannot be
// reached is skipped; an answer, even an error, ends the search.
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
		// a write goes to the next endpoint only if it never left: one that was
		// sent may have committed even though no answer came back
		var sent atomic.Bool // set in the transport's goroutine
		request = request.WithContext(httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
			WroteRequest: func(httptrace.WroteRequestInfo) { sent.Store(true) },
		}))
		err = send(c.http, request, out)
		var statusError *StatusError
		switch {
		case err == nil || errors.As(err, &statusError):
			// any answer, an error too, shows the endpoint is reachable: all
			// that inTurn goes by
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

// isForeignCA tells whether an agent showed a certificate of a CA other
// than the one in the user's credentials.
func isForeignCA(err error) bool {
	var unknown x509.UnknownAuthorityError
	return errors.As(err, &unknown)
}

// foreignCA says what a foreign CA most likely means, instead of x509's
// words alone. The next endpoint is not tried: it belongs to the same cluster.
func foreignCA(endpoint string, err error) error {
	return fmt.Errorf("%s belongs to another cluster than your credentials: a cluster of the same name, "+
		"or one set up anew? (%w)", endpoint, err)
}

// statusError reads an answer that is not 2xx: the API's Error, or the text.
func statusError(response *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20)) // for the message only
	var answer Error
	if json.Unmarshal(body, &answer) != nil || answer.Message == "" {
		answer.Message = strings.TrimSpace(string(body))
	}
	return &StatusError{Status: response.StatusCode, Message: answer.Message}
}

// send runs a request and decodes a 2xx answer into out; any other answer
// becomes a StatusError.
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
