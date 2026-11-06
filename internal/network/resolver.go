package network

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/miekg/dns"
)

// upstreamTimeout bounds one question to an upstream resolver.
const upstreamTimeout = 3 * time.Second

// DNSResolver answers the containers on Resolver:53 over UDP and TCP. For
// now it passes every question on to the host's resolvers. The names of
// exported services (<service>.<stack>.clowder) come with exports.
type DNSResolver struct {
	upstreams []string
	servers   []*dns.Server
	logger    *slog.Logger
}

// Upstreams reads the host's resolvers from resolv.conf. A local stub
// (127.0.0.53) works, because the resolver runs on the host, not in a
// container.
func Upstreams(resolvConf string) ([]string, error) {
	config, err := dns.ClientConfigFromFile(resolvConf)
	if err != nil {
		return nil, fmt.Errorf("read the host's resolvers: %w", err)
	}
	var upstreams []string
	for _, server := range config.Servers {
		upstreams = append(upstreams, net.JoinHostPort(server, config.Port))
	}
	if len(upstreams) == 0 {
		return nil, fmt.Errorf("%s names no resolver", resolvConf)
	}
	return upstreams, nil
}

// StartResolver listens on address (host:port) and answers until Close.
func StartResolver(address string, upstreams []string, logger *slog.Logger) (*DNSResolver, error) {
	resolver := &DNSResolver{upstreams: upstreams, logger: logger}
	for _, network := range []string{"udp", "tcp"} {
		started := make(chan struct{})
		server := &dns.Server{
			Addr: address, Net: network, Handler: dns.HandlerFunc(resolver.answer),
			NotifyStartedFunc: func() { close(started) },
		}
		failed := make(chan error, 1)
		go func() { failed <- server.ListenAndServe() }()
		select {
		case <-started:
			resolver.servers = append(resolver.servers, server)
		case err := <-failed:
			_ = resolver.Close() // the listen error is the one to report
			return nil, fmt.Errorf("start the DNS resolver on %s/%s: %w", address, network, err)
		}
	}
	return resolver, nil
}

// answer passes a question on to the first upstream that answers. It uses
// the protocol that the question came in with. A large answer over UDP
// comes back truncated, and the container asks again over TCP, as with any
// resolver.
func (r *DNSResolver) answer(w dns.ResponseWriter, question *dns.Msg) {
	client := &dns.Client{Net: w.LocalAddr().Network(), Timeout: upstreamTimeout}
	var lastErr error
	for _, upstream := range r.upstreams {
		reply, _, err := client.Exchange(question, upstream)
		if err == nil {
			_ = w.WriteMsg(reply) // the container is gone if this fails
			return
		}
		lastErr = err
	}
	r.logger.Warn("no upstream resolver answered", "question", question.Question, "error", lastErr)
	failure := new(dns.Msg)
	failure.SetRcode(question, dns.RcodeServerFailure)
	_ = w.WriteMsg(failure)
}

// Close stops the resolver.
func (r *DNSResolver) Close() error {
	var errs []error
	for _, server := range r.servers {
		errs = append(errs, server.Shutdown())
	}
	return errors.Join(errs...)
}
