// Package notify sends alerts to the channels cluster.yaml names: Discord,
// email and webhooks. Only the leader's controller uses it; it knows
// nothing of the state, and gets each channel's secrets opened.
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
)

// The events a message tells of.
const (
	Raised   = "raised"
	Reminder = "reminder"
	Resolved = "resolved"
	Test     = "test"
)

// Message is one alert's event as a channel sends it; the webhook posts it
// as it is.
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

// title is the one-line summary that heads the message everywhere.
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

// Secrets opens a secret of cluster.yaml by its name.
type Secrets func(name string) (string, error)

// New makes the notifier of a channel, with its secrets opened and the
// files next to cluster.yaml (smtp.ca) at hand.
func New(channel clusterspec.AlertChannel, secrets Secrets, files map[string][]byte) (Notifier, error) {
	switch channel.Type {
	case "discord":
		url, err := secrets(channel.Webhook.Secret)
		return discord{url: url}, err
	case "webhook":
		url, err := secrets(channel.URL.Secret)
		return webhook{url: url}, err
	case "email":
		return newEmail(channel, secrets, files)
	default:
		return nil, fmt.Errorf("alert channel type %q is not supported", channel.Type)
	}
}

// postJSON posts body as JSON to target and wants a 2xx answer.
func postJSON(ctx context.Context, target string, body any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(encoded))
	if err != nil {
		// the URL is a secret: no word of it in the error
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

// discord posts the message to a Discord webhook as an embed, colored by
// severity, green for the all-clear.
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

// Heartbeat calls the heartbeat's URL (a GET, as services such as
// healthchecks.io take it); the service alarms when the calls stop.
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

// trust is the system's CAs and the one of a file, if any.
func trust(caPEM []byte) (*x509.CertPool, error) {
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool() // no system pool: only the file, if any
	}
	if len(caPEM) > 0 && !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("the CA file holds no PEM certificate")
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
