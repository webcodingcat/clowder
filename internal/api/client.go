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
	"net/http"
	"net/http/httptrace"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/webcodingcat/clowder/internal/pki"
)

// Client is the CLI's connection to the cluster. It tries the seeds in turn
// until one answers, and after a write it asks reads to wait for that write,
// whichever agent answers them.
type Client struct {
	seeds []string
	// http answers within its timeout; streaming has none (logs that follow)
	http      *http.Client
	streaming *http.Client
	version   string
	// written is the index of this client's last write.
	written uint64
}

// NewClient connects with a user's certificate to the agents at seeds.
func NewClient(seeds []string, certificate tls.Certificate, ca *x509.Certificate, version string) *Client {
	transport := &http.Transport{TLSClientConfig: pki.ClientTLSToHosts(certificate, ca, nil)}
	return &Client{
		seeds:     seeds,
		version:   version,
		http:      &http.Client{Timeout: 30 * time.Second, Transport: transport},
		streaming: &http.Client{Transport: transport},
	}
}

// Get reads path into out.
func (c *Client) Get(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodGet, path, nil, out)
}

// Post writes in (nil for none) to path. Reads after it wait for the write.
func (c *Client) Post(ctx context.Context, path string, in any) (Written, error) {
	return c.write(ctx, http.MethodPost, path, in)
}

// Act posts in (nil for none) to path and reads the answer into out: an
// action that writes nothing to the state, such as a test alert.
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

// Put replaces what is at path with in. Reads after it wait for the write.
func (c *Client) Put(ctx context.Context, path string, in any) (Written, error) {
	return c.write(ctx, http.MethodPut, path, in)
}

// Delete removes what is at path. Reads after it wait for the write.
func (c *Client) Delete(ctx context.Context, path string) (Written, error) {
	return c.write(ctx, http.MethodDelete, path, nil)
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
	return err
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

// openStream asks the first seed that answers for path, without a timeout:
// the answer streams until ctx ends. An answer other than 2xx becomes a
// StatusError.
func (c *Client) openStream(ctx context.Context, path string) (io.ReadCloser, error) {
	var unreachable []string
	for _, seed := range c.seeds {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+Address(seed)+path, nil)
		if err != nil {
			return nil, err
		}
		request.Header.Set(VersionHeader, c.version)
		response, err := c.streaming.Do(request)
		if isForeignCA(err) {
			return nil, foreignCA(seed, err)
		}
		if err != nil {
			unreachable = append(unreachable, fmt.Sprintf("%s: %v", seed, err))
			continue
		}
		if response.StatusCode >= 300 {
			defer func() { _ = response.Body.Close() }() // read for the message
			return nil, statusError(response)
		}
		return response.Body, nil
	}
	return nil, fmt.Errorf("no agent answered:\n  %s", strings.Join(unreachable, "\n  "))
}

func (c *Client) write(ctx context.Context, method, path string, in any) (Written, error) {
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

// do sends a request to the first seed that answers. A seed that cannot be
// reached is skipped; an answer, even an error, ends the search.
func (c *Client) do(ctx context.Context, method, path string, body []byte, out any) error {
	var unreachable []string
	for _, seed := range c.seeds {
		request, err := http.NewRequestWithContext(ctx, method, "https://"+Address(seed)+path, bytes.NewReader(body))
		if err != nil {
			return err
		}
		request.Header.Set(VersionHeader, c.version)
		if method == http.MethodGet && c.written > 0 {
			request.Header.Set(MinIndexHeader, strconv.FormatUint(c.written, 10))
		}
		// a write goes to the next seed only if it never left: one that was
		// sent may have committed even though no answer came back
		var sent atomic.Bool // set in the transport's goroutine
		request = request.WithContext(httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
			WroteRequest: func(httptrace.WroteRequestInfo) { sent.Store(true) },
		}))
		err = send(c.http, request, out)
		var statusError *StatusError
		switch {
		case err == nil || errors.As(err, &statusError):
			return err
		case isForeignCA(err):
			return foreignCA(seed, err)
		case sent.Load() && method != http.MethodGet:
			return fmt.Errorf("%s: %w (the write may have gone through: look before you repeat it)", seed, err)
		}
		unreachable = append(unreachable, fmt.Sprintf("%s: %v", seed, err))
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
// words alone. The next seed is not tried: it belongs to the same cluster.
func foreignCA(seed string, err error) error {
	return fmt.Errorf("%s belongs to another cluster than your credentials: a cluster of the same name, "+
		"or one set up anew? (%w)", seed, err)
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
