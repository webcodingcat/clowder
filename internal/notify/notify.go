// Package notify sends alerts to the channels that the configuration names:
// Discord, email and webhooks. Only the leader's controller uses it. It
// knows nothing about the state, and it receives each channel's secrets
// already opened.
package notify

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
	"net/url"
	"time"

	"github.com/webcodingcat/clowder/internal/clusterspec"
	"github.com/webcodingcat/clowder/internal/names"
)

// The events that a message reports.
const (
	Raised   = "raised"
	Reminder = "reminder"
	Resolved = "resolved"
	Test     = "test"
)

// Message is one event of an alert, as a channel sends it. The webhook
// posts it unchanged.
type Message struct {
	Cluster  string    `json:"cluster"`
	Event    string    `json:"event"`
	Key      string    `json:"key"`
	Kind     string    `json:"kind"`
	Severity string    `json:"severity"`
	Subject  string    `json:"subject"`
	Text     string    `json:"text"`
	Since    time.Time `json:"since"`
}

// title is the one-line summary at the top of the message in every channel.
func (m Message) title() string {
	switch m.Event {
	case Resolved:
		return fmt.Sprintf("[%s] resolved: %s", m.Cluster, m.Subject)
	case Reminder:
		return fmt.Sprintf("[%s] %s, still: %s", m.Cluster, m.Severity, m.Subject)
	default:
		return fmt.Sprintf("[%s] %s: %s", m.Cluster, m.Severity, m.Subject)
	}
}

// Notifier sends messages to one channel.
type Notifier interface {
	Send(ctx context.Context, message Message) error
}

// sendTimeout bounds one message to one channel.
const sendTimeout = 30 * time.Second

// Secrets opens a value that the configuration names, from a Secret in
// clowder-system.
type Secrets func(ref names.SecretKeyRef) (string, error)

// New creates the notifier of a channel and opens the channel's secrets.
func New(channel clusterspec.AlertChannel, secrets Secrets) (Notifier, error) {
	switch {
	case channel.Type == "discord" && channel.WebhookSecretRef != nil:
		url, err := secrets(*channel.WebhookSecretRef)
		return discord{url: url}, err
	case channel.Type == "webhook" && channel.URLSecretRef != nil:
		url, err := secrets(*channel.URLSecretRef)
		return webhook{url: url}, err
	case channel.Type == "email" && channel.SMTP != nil:
		return newEmail(channel, secrets)
	default:
		return nil, fmt.Errorf("alert channel type %q is not supported", channel.Type)
	}
}

// postJSON posts body as JSON to target and expects a 2xx answer.
func postJSON(ctx context.Context, target string, body any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(encoded))
	if err != nil {
		// the URL is a secret, so the error must not contain any of it
		return errors.New("the channel's URL does not read")
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		var urlError *url.Error
		if errors.As(err, &urlError) {
			err = urlError.Err // url.Error names the URL
		}
		return fmt.Errorf("post to the channel: %w", err)
	}
	defer func() { _ = response.Body.Close() }() // read below
	answer, _ := io.ReadAll(io.LimitReader(response.Body, 512))
	if response.StatusCode/100 != 2 {
		return fmt.Errorf("the channel answered HTTP %d: %s", response.StatusCode, answer)
	}
	return nil
}

// webhook posts the message as JSON.
type webhook struct {
	url string
}

func (w webhook) Send(ctx context.Context, message Message) error {
	return postJSON(ctx, w.url, message)
}

// discord posts the message to a Discord webhook as an embed. The color
// shows the severity, and green means the all-clear.
type discord struct {
	url string
}

func (d discord) Send(ctx context.Context, message Message) error {
	colors := map[string]int{
		clusterspec.SeverityInfo: 0x1976D2, clusterspec.SeverityWarning: 0xF9A825, clusterspec.SeverityCritical: 0xD32F2F,
	}
	color := colors[message.Severity]
	if message.Event == Resolved {
		color = 0x388E3C
	}
	type embed struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		Color       int    `json:"color"`
	}
	return postJSON(ctx, d.url, map[string][]embed{
		"embeds": {{Title: message.title(), Description: message.Text, Color: color}},
	})
}

// Heartbeat calls the heartbeat's URL (with a GET, as services such as
// healthchecks.io expect); the service raises an alarm when the calls stop.
func Heartbeat(ctx context.Context, target string) error {
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, http.NoBody)
	if err != nil {
		return errors.New("the heartbeat's URL does not read")
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		var urlError *url.Error
		if errors.As(err, &urlError) {
			err = urlError.Err // url.Error names the URL
		}
		return fmt.Errorf("call the heartbeat: %w", err)
	}
	defer func() { _ = response.Body.Close() }() // only the status counts
	if response.StatusCode/100 != 2 {
		return fmt.Errorf("the heartbeat answered HTTP %d", response.StatusCode)
	}
	return nil
}

// trust returns the system's CAs plus the CA from caPEM, if one is given.
func trust(caPEM []byte) (*x509.CertPool, error) {
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool() // no system pool: only the CA given, if any
	}
	if len(caPEM) > 0 && !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("the CA holds no PEM certificate")
	}
	return pool, nil
}

// tlsConfig is how the email channel speaks TLS with its server.
func tlsConfig(host string, caPEM []byte) (*tls.Config, error) {
	pool, err := trust(caPEM)
	if err != nil {
		return nil, err
	}
	return &tls.Config{ServerName: host, RootCAs: pool, MinVersion: tls.VersionTLS12}, nil
}
