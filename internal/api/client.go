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
	"strconv"
	"strings"
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
			Transport: &http.Transport{TLSClientConfig: clientTLS(certificate, ca)},
		},
	}
}

// clientTLS trusts any host of the cluster's CA, whatever name or address it
// was reached by: seeds may be names, and a host certificate lists only the
// host's addresses.
func clientTLS(certificate tls.Certificate, ca *x509.Certificate) *tls.Config {
	return &tls.Config{
		Certificates:       []tls.Certificate{certificate},
		InsecureSkipVerify: true, //nolint:gosec // VerifyConnection checks the chain instead of the name
		VerifyConnection: func(connection tls.ConnectionState) error {
			if len(connection.PeerCertificates) == 0 {
				return errors.New("the agent showed no certificate")
			}
			identity, err := pki.Verify(ca, connection.PeerCertificates[0], func(string) bool { return false }, time.Now())
			if err != nil {
				return fmt.Errorf("the agent's certificate: %w", err)
			}
			if identity.Role != pki.RoleHost {
				return fmt.Errorf("%s is no host of the cluster", identity.Name)
			}
			return nil
		},
		MinVersion: tls.VersionTLS13,
	}
}

// Get reads path into out.
func (c *Client) Get(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodGet, path, nil, out)
}

// Post writes in (nil for none) to path. Reads after it wait for the write.
func (c *Client) Post(ctx context.Context, path string, in any) (Written, error) {
	var body []byte
	if in != nil {
		encoded, err := json.Marshal(in)
		if err != nil {
			return Written{}, fmt.Errorf("encode the request: %w", err)
		}
		body = encoded
	}
	var written Written
	if err := c.do(ctx, http.MethodPost, path, body, &written); err != nil {
		return written, err
	}
	c.written = max(c.written, written.Index)
	return written, nil
}

// do sends a request to the first seed that answers. Only a seed that cannot
// be reached is skipped; an answer, even an error, ends the search.
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
		err = send(c.http, request, out)
		var statusError *StatusError
		if err != nil && !errors.As(err, &statusError) {
			unreachable = append(unreachable, fmt.Sprintf("%s: %v", seed, err))
			continue
		}
		return err
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
