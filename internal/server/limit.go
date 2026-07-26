package server

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// parseCIDRs parses a list of CIDR strings into networks, skipping any that don't
// parse (they are simply not trusted).
func parseCIDRs(cidrs []string) []*net.IPNet {
	var out []*net.IPNet
	for _, c := range cidrs {
		if _, n, err := net.ParseCIDR(strings.TrimSpace(c)); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// clientIP resolves the request's client IP for fairness/throttling. When the
// immediate peer (RemoteAddr) is a trusted proxy, it trusts the rightmost
// X-Forwarded-For entry — the address that proxy actually observed — which a
// client cannot spoof past a correctly-configured Caddy/Traefik. Otherwise it uses
// the peer address directly. Returns "" only if nothing parses.
func clientIP(r *http.Request, trusted []*net.IPNet) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer := net.ParseIP(host)
	if peer != nil && ipInAny(peer, trusted) {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			// Rightmost entry is the address the trusted proxy saw.
			cand := strings.TrimSpace(parts[len(parts)-1])
			if net.ParseIP(cand) != nil {
				return cand
			}
		}
	}
	return host
}

func ipInAny(ip net.IP, nets []*net.IPNet) bool {
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// ipLimiter is a per-IP token-bucket rate limiter. Tokens refill at ratePerSec up
// to burst; each allowed request consumes one. A ratePerSec <= 0 disables limiting
// (allow always). Stale buckets are evicted periodically.
type ipLimiter struct {
	mu         sync.Mutex
	buckets    map[string]*bucket
	ratePerSec float64
	burst      float64
}

type bucket struct {
	tokens float64
	last   time.Time
}

// newIPLimiter builds a limiter from a per-minute rate and burst. ratePerMin <= 0
// yields a no-op limiter (allow always).
func newIPLimiter(ratePerMin, burst int) *ipLimiter {
	l := &ipLimiter{buckets: map[string]*bucket{}}
	if ratePerMin > 0 {
		l.ratePerSec = float64(ratePerMin) / 60
		l.burst = float64(burst)
		if l.burst < 1 {
			l.burst = 1
		}
	}
	return l
}

// allow reports whether a request from ip may proceed now, consuming a token.
func (l *ipLimiter) allow(ip string) bool {
	if l.ratePerSec <= 0 {
		return true // limiting disabled
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	b := l.buckets[ip]
	if b == nil {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[ip] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * l.ratePerSec
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

// gc evicts buckets untouched for longer than maxIdle, bounding memory under a
// churn of distinct IPs.
func (l *ipLimiter) gc(maxIdle time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := time.Now().Add(-maxIdle)
	for ip, b := range l.buckets {
		if b.last.Before(cutoff) {
			delete(l.buckets, ip)
		}
	}
}
