package api

import (
	"errors"
	"net/http"
	"net/netip"
	"sync"
	"time"
)

// How many tries an agent accepts per minute on a route without a client
// certificate (the join's routes). The limit per sender keeps a single
// stranger from locking everybody out. The total limit keeps many senders
// from keeping the agent busy, because each such route works with the data
// key for anyone who asks. Someone with many addresses can still block the
// route for minutes, but no data is at risk then.
const (
	triesPerSender = 10
	triesInAll     = 100
)

// limiter counts the tries of the current minute in memory. Each agent
// counts for itself. A refused try is not counted, so the map never holds
// more than triesInAll senders, however many there are.
type limiter struct {
	mu       sync.Mutex
	minute   int64
	bySender map[netip.Addr]int
	total    int
}

// allow tells whether a try by sender at now is accepted, and counts it.
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

// senderOf returns the address that a request counts for: the IPv4
// address, or for IPv6 the /64, because one machine usually has a whole
// /64 to itself.
func senderOf(r *http.Request) netip.Addr {
	addressPort, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{} // not over TCP (tests); such callers share one count
	}
	address := addressPort.Addr().Unmap()
	if address.Is6() {
		prefix, _ := address.Prefix(64) // cannot fail for an IPv6 address
		return prefix.Addr()
	}
	return address
}

// limited lets a route accept only the tries that the limiter allows. The
// others get 429.
func limited(tries *limiter, handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !tries.allow(senderOf(r), time.Now()) {
			writeError(w, http.StatusTooManyRequests, errors.New("too many tries; wait a minute"))
			return
		}
		handler(w, r)
	}
}
