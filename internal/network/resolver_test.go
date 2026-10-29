package network

import (
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/miekg/dns"
)

// startUpstream runs a resolver that knows one name.
func startUpstream(t *testing.T) string {
	t.Helper()
	address := freeUDPAddress(t)
	started := make(chan struct{})
	upstream := &dns.Server{
		Addr: address, Net: "udp", NotifyStartedFunc: func() { close(started) },
		Handler: dns.HandlerFunc(func(w dns.ResponseWriter, question *dns.Msg) {
			reply := new(dns.Msg)
			reply.SetReply(question)
			if question.Question[0].Name == "registry.example.com." {
				record, _ := dns.NewRR("registry.example.com. 60 IN A 192.0.2.7")
				reply.Answer = append(reply.Answer, record)
			} else {
				reply.SetRcode(question, dns.RcodeNameError)
			}
			_ = w.WriteMsg(reply)
		}),
	}
	go func() { _ = upstream.ListenAndServe() }()
	<-started
	t.Cleanup(func() { _ = upstream.Shutdown() })
	return address
}

func freeUDPAddress(t *testing.T) string {
	t.Helper()
	connection, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := connection.LocalAddr().String()
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

func TestTheResolverPassesQuestionsOn(t *testing.T) {
	dead := freeUDPAddress(t) // nobody answers here: the next upstream must
	resolver, err := StartResolver("127.0.0.1:0", []string{dead, startUpstream(t)},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resolver.Close() }()
	address := resolver.servers[0].PacketConn.LocalAddr().String()

	question := new(dns.Msg)
	question.SetQuestion("registry.example.com.", dns.TypeA)
	reply, err := dns.Exchange(question, address)
	if err != nil {
		t.Fatal(err)
	}
	if len(reply.Answer) != 1 || reply.Answer[0].(*dns.A).A.String() != "192.0.2.7" {
		t.Errorf("answer %v", reply.Answer)
	}
	question.SetQuestion("nothing.example.com.", dns.TypeA)
	if reply, err := dns.Exchange(question, address); err != nil || reply.Rcode != dns.RcodeNameError {
		t.Errorf("an unknown name: %v, %v", reply, err)
	}
}

func TestUpstreamsComeFromResolvConf(t *testing.T) {
	path := filepath.Join(t.TempDir(), "resolv.conf")
	if err := os.WriteFile(path, []byte("nameserver 185.12.64.1\nnameserver 185.12.64.2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	upstreams, err := Upstreams(path)
	if err != nil || len(upstreams) != 2 || upstreams[0] != "185.12.64.1:53" {
		t.Errorf("upstreams %v, %v", upstreams, err)
	}
}
