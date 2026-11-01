package api

import (
	"errors"
	"net/http"
	"net/netip"
	"sync"
	"time"
)

// How many tries of a recovery an agent takes per minute: per sender, so a
// stranger cannot lock everybody out; in all, so many senders cannot keep
// the agent busy. Someone with many addresses can still block recoveries
// for minutes; no data is at risk then.
const (
	triesPerSender = 10
	triesInAll     = 100
)

// limiter counts the tries of the current minute, in memory: each agent
// for itself. Only tries it takes are counted, so it never holds more than
// triesInAll senders.
type limiter struct {
	mu       sync.Mutex
	minute   int64
	bySender map[netip.Addr]int
	total    int
}

// allow tells whether a try of sender at now is taken, and counts it.
func (l *limiter) allow(sender netip.Addr, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if minute := now.Unix() / 60; minute != l.minute || l.bySender == nil {
		l.minute, l.bySender, l.total = minute, map[netip.Addr]int{}, 0
	}
	if l.total >= triesInAll || l.bySender[sender] >= triesPerSender {
		return false
	}
	l.bySender[sender]++
	l.total++
	return true
}

// senderOf is the address a request counts for: the IPv4 address, or for
// IPv6 the /64, which one machine usually has whole.
func senderOf(r *http.Request) netip.Addr {
	addressPort, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{} // not over TCP (tests); such callers share one count
	}
	address := addressPort.Addr().Unmap()
	if address.Is6() {
		prefix, _ := address.Prefix(64) // an IPv6 address has 64 bits to keep
		return prefix.Addr()
	}
	return address
}

// limited lets a route take only the tries the limiter allows; the rest
// get 429.
func (s *Server) limited(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.recoveries.allow(senderOf(r), time.Now()) {
			writeError(w, http.StatusTooManyRequests, errors.New("too many tries of a recovery; wait a minute"))
			return
		}
		handler(w, r)
	}
}
