package notify

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"github.com/webcodingcat/clowder/internal/clusterspec"
)

// implicitTLSPort is SMTP's port that speaks TLS from the start (RFC 8314);
// any other port must offer STARTTLS, so no password and no alert travels
// in clear.
const implicitTLSPort = 465

// email sends the message as a plain-text mail.
type email struct {
	host     string
	port     int
	tls      *tls.Config
	user     string
	password string
	from     string
	to       []string
}

func newEmail(channel clusterspec.AlertChannel, secrets Secrets, files map[string][]byte) (email, error) {
	server := channel.SMTP
	var caPEM []byte
	if server.CA != "" {
		caPEM = files[server.CA]
	}
	config, err := tlsConfig(server.Host, caPEM)
	if err != nil {
		return email{}, err
	}
	sender := email{
		host: server.Host, port: server.Port, tls: config, user: server.User, from: channel.From, to: channel.To,
	}
	if server.Password != nil {
		if sender.password, err = secrets(server.Password.Secret); err != nil {
			return email{}, err
		}
	}
	return sender, nil
}

func (e email) Send(ctx context.Context, message Message) error {
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	client, err := e.connect(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }() // Quit below says goodbye; this only frees
	if e.user != "" {
		if err := client.Auth(smtp.PlainAuth("", e.user, e.password, e.host)); err != nil {
			return fmt.Errorf("log in to the mail server: %w", err)
		}
	}
	if err := client.Mail(e.from); err != nil {
		return fmt.Errorf("send the mail: %w", err)
	}
	for _, to := range e.to {
		if err := client.Rcpt(to); err != nil {
			return fmt.Errorf("send the mail to %s: %w", to, err)
		}
	}
	mail, err := e.mail(message)
	if err != nil {
		return err
	}
	body, err := client.Data()
	if err != nil {
		return fmt.Errorf("send the mail: %w", err)
	}
	if _, err := body.Write(mail); err != nil {
		return fmt.Errorf("send the mail: %w", err)
	}
	if err := body.Close(); err != nil {
		return fmt.Errorf("send the mail: %w", err)
	}
	return client.Quit()
}

// connect reaches the server and makes sure the talk is encrypted: TLS from
// the start on port 465, else STARTTLS, which the server must offer.
func (e email) connect(ctx context.Context) (*smtp.Client, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(e.host, strconv.Itoa(e.port)))
	if err != nil {
		return nil, fmt.Errorf("reach the mail server: %w", err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline) // a conn of the dialer always takes a deadline
	}
	if e.port == implicitTLSPort {
		conn = tls.Client(conn, e.tls)
	}
	client, err := smtp.NewClient(conn, e.host)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("talk to the mail server: %w", err)
	}
	if _, isTLS := client.TLSConnectionState(); !isTLS {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			_ = client.Close()
			return nil, errors.New("the mail server offers no STARTTLS; clowder sends no mail in clear")
		}
		if err := client.StartTLS(e.tls); err != nil {
			_ = client.Close()
			return nil, fmt.Errorf("start TLS with the mail server: %w", err)
		}
	}
	return client, nil
}

// mail is the message as a mail: headers, a blank line, the text, with
// CRLF line ends. The headers are those strict providers want; the
// subject is encoded (RFC 2047), as a stack's name may hold more than
// ASCII.
func (e email) mail(message Message) ([]byte, error) {
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return nil, err
	}
	_, domain, _ := strings.Cut(e.from, "@")
	lines := []string{
		"From: " + e.from,
		"To: " + strings.Join(e.to, ", "),
		"Subject: " + mime.QEncoding.Encode("utf-8", message.title()),
		"Date: " + time.Now().UTC().Format(time.RFC1123Z),
		"Message-ID: <" + hex.EncodeToString(id) + "@" + domain + ">",
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=utf-8",
		"",
	}
	lines = append(lines, strings.Split(strings.ReplaceAll(message.Text, "\r\n", "\n"), "\n")...)
	return []byte(strings.Join(lines, "\r\n") + "\r\n"), nil
}
