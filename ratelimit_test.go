package main

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestClientIP(t *testing.T) {
	loopback, _ := parseProxies("loopback")
	private, _ := parseProxies("private, 203.0.113.7")
	cases := []struct {
		name    string
		peer    string
		header  string
		values  []string
		trusted bool // use "private, 203.0.113.7" instead of "loopback"
		want    string
	}{
		{"direct, no header", "198.51.100.1:5000", "X-Forwarded-For", nil, false, "198.51.100.1"},
		{"direct client faking the header is ignored", "198.51.100.1:5000", "X-Forwarded-For", []string{"1.1.1.1"}, false, "198.51.100.1"},
		{"local proxy", "127.0.0.1:5000", "X-Forwarded-For", []string{"198.51.100.9"}, false, "198.51.100.9"},
		{"client-written entries on the left are skipped", "127.0.0.1:5000", "X-Forwarded-For", []string{"6.6.6.6, 198.51.100.9"}, false, "198.51.100.9"},
		{"chain of trusted proxies", "10.0.0.2:80", "X-Forwarded-For", []string{"6.6.6.6, 198.51.100.9, 203.0.113.7", "10.0.0.5"}, true, "198.51.100.9"},
		{"everything trusted: leftmost", "10.0.0.2:80", "X-Forwarded-For", []string{"192.168.1.20, 10.0.0.5"}, true, "192.168.1.20"},
		{"garbage stops the walk", "127.0.0.1:1", "X-Forwarded-For", []string{"nonsense, 198.51.100.9"}, false, "198.51.100.9"},
		{"garbage only: the proxy", "127.0.0.1:1", "X-Forwarded-For", []string{"nonsense"}, false, "127.0.0.1"},
		{"entries with ports", "127.0.0.1:1", "X-Forwarded-For", []string{"[2001:db8::1]:443"}, false, "2001:db8::"},
		{"single-value header", "127.0.0.1:1", "CF-Connecting-IP", []string{"198.51.100.4"}, false, "198.51.100.4"},
		{"single-value header from a stranger", "198.51.100.1:1", "CF-Connecting-IP", []string{"1.1.1.1"}, false, "198.51.100.1"},
		{"IPv6 grouped by /64", "[2001:db8:1:2:3:4:5:6]:1", "", nil, false, "2001:db8:1:2::"},
	}
	for _, c := range cases {
		ips := clientIPs{header: c.header, trusted: loopback}
		if c.trusted {
			ips.trusted = private
		}
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.peer
		for _, v := range c.values {
			r.Header.Add(c.header, v)
		}
		if got := ips.of(r); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

func TestParseProxies(t *testing.T) {
	for _, ok := range []string{"", "none", "loopback", "private", "10.1.2.3", "2001:db8::1", "173.245.48.0/20, loopback"} {
		if _, err := parseProxies(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"everyone", "10.0.0.0/33", "300.1.1.1"} {
		if _, err := parseProxies(bad); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
}

func TestRateLimitOff(t *testing.T) {
	var l *limiter // what NewAPI uses with -rate-limit=false
	now := time.Now()
	for i := 0; i < 1000; i++ {
		if !l.allow("x", now) || !l.peek("x", now) {
			t.Fatal("a nil limiter must allow everything")
		}
	}
	l.sweep(now)
	if a := NewAPI(Config{RateLimit: false}, nil, Signer{}); a.general != nil || a.mint != nil || a.badAuth != nil {
		t.Error("-rate-limit=false should turn every limiter off")
	}
}
