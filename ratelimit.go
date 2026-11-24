package main

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// limiter is a token bucket per key (client IP), kept in memory: not in SQLite, not shared between
// instances, empty again after a restart. Small and dependency-free.
// Each key may spend [burst] requests at once and gets [perSecond] new ones every second.
// A nil limiter (rate limits turned off) allows everything.
type limiter struct {
	mu        sync.Mutex
	buckets   map[string]*bucket
	perSecond float64
	burst     float64
	maxKeys   int
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newLimiter(perSecond, burst float64) *limiter {
	return &limiter{buckets: map[string]*bucket{}, perSecond: perSecond, burst: burst, maxKeys: 200_000}
}

// allow takes one token for key, if there is one.
func (l *limiter) allow(key string, now time.Time) bool {
	if l == nil {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) >= l.maxKeys {
			return false // under a flood of new addresses, refuse rather than run out of memory
		}
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.perSecond)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// peek reports whether key has a token left, without taking it.
func (l *limiter) peek(key string, now time.Time) bool {
	if l == nil {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[key]
	if !ok {
		return true
	}
	return min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.perSecond) >= 1
}

// sweep forgets keys whose bucket is full again (they behave like new ones anyway).
func (l *limiter) sweep(now time.Time) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, b := range l.buckets {
		if b.tokens+now.Sub(b.last).Seconds()*l.perSecond >= l.burst {
			delete(l.buckets, k)
		}
	}
}

// clientIPs finds the address to rate-limit by.
//
// Behind a reverse proxy every request comes from the proxy, so the real client is in a header
// the proxy sets. That header is believed only when the request comes from a trusted proxy
// (-trusted-proxies): anyone else could write whatever they like in it.
//
// X-Forwarded-For is a list that each proxy appends to ("client, proxy1, proxy2"). It is read from
// the right, skipping our own trusted proxies; the first other address is the client. Entries
// further left were sent by the client itself and are never used. Single-value headers
// (CF-Connecting-IP, X-Real-IP) are overwritten by the proxy and taken as they are.
type clientIPs struct {
	header  string
	trusted []*net.IPNet
}

func (c clientIPs) of(r *http.Request) string {
	peer := parseIP(r.RemoteAddr)
	if peer == nil {
		return r.RemoteAddr
	}
	if c.header == "" || !c.isTrusted(peer) {
		return ipKey(peer)
	}
	values := r.Header.Values(c.header)
	if len(values) == 0 {
		return ipKey(peer)
	}
	if !strings.EqualFold(c.header, "X-Forwarded-For") {
		if ip := parseIP(values[len(values)-1]); ip != nil {
			return ipKey(ip)
		}
		return ipKey(peer)
	}
	hops := strings.Split(strings.Join(values, ","), ",")
	client := peer
	for i := len(hops) - 1; i >= 0; i-- {
		ip := parseIP(hops[i])
		if ip == nil {
			break // garbage: keep the last good hop
		}
		client = ip
		if !c.isTrusted(ip) {
			break
		}
	}
	return ipKey(client)
}

func (c clientIPs) isTrusted(ip net.IP) bool {
	for _, n := range c.trusted {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// parseIP accepts "1.2.3.4", "1.2.3.4:5678", "::1" and "[::1]:5678".
func parseIP(s string) net.IP {
	s = strings.TrimSpace(s)
	if ip := net.ParseIP(s); ip != nil {
		return ip
	}
	if host, _, err := net.SplitHostPort(s); err == nil {
		return net.ParseIP(host)
	}
	return nil
}

// ipKey groups IPv6 addresses by /64 (one home or phone usually owns a whole /64).
func ipKey(ip net.IP) string {
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.Mask(net.CIDRMask(64, 128)).String()
}
