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
	seeds   []string
	http    *http.Client
	version string
	// written is the index of this client's last write.
	written uint64
}

// NewClient connects with a user's certificate to the agents at seeds.
func NewClient(seeds []string, certificate tls.Certificate, ca *x509.Certificate, version string) *Client {
	return &Client{
		seeds:   seeds,
		version: version,
		http: &http.Client{
			Timeout:   30 * time.Second,
			Transport: &http.Transport{TLSClientConfig: pki.HostClientTLS(certificate, ca, nil)},
		},
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

// Put replaces what is at path with in. Reads after it wait for the write.
func (c *Client) Put(ctx context.Context, path string, in any) (Written, error) {
	return c.write(ctx, http.MethodPut, path, in)
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
		case sent.Load() && method != http.MethodGet:
			return fmt.Errorf("%s: %w (the write may have gone through: look before you repeat it)", seed, err)
		}
		unreachable = append(unreachable, fmt.Sprintf("%s: %v", seed, err))
	}
	return fmt.Errorf("no agent answered:\n  %s", strings.Join(unreachable, "\n  "))
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
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return fmt.Errorf("read the answer: %w", err)
	}
	if response.StatusCode >= 300 {
		var answer Error
		if json.Unmarshal(body, &answer) != nil || answer.Message == "" {
			answer.Message = strings.TrimSpace(string(body))
		}
		return &StatusError{Status: response.StatusCode, Message: answer.Message}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("read the answer: %w", err)
	}
	return nil
}
