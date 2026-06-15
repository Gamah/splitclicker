package api

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// rateLimiter is a per-client-IP token bucket for the unauthenticated,
// Facepunch-proxying /auth endpoint: without it, anyone could amplify our
// outbound Facepunch calls or spam player-row upserts. Each bucket refills at
// `rate` tokens/sec up to `burst`; an empty bucket yields HTTP 429.
type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*ipBucket
	rate    float64
	burst   float64
}

type ipBucket struct {
	tokens float64
	last   time.Time
}

func newRateLimiter(perSecond, burst float64) *rateLimiter {
	rl := &rateLimiter{buckets: map[string]*ipBucket{}, rate: perSecond, burst: burst}
	go rl.sweepLoop()
	return rl
}

func (rl *rateLimiter) allow(ip string) bool {
	now := time.Now()
	rl.mu.Lock()
	defer rl.mu.Unlock()
	b, ok := rl.buckets[ip]
	if !ok {
		rl.buckets[ip] = &ipBucket{tokens: rl.burst - 1, last: now}
		return true
	}
	b.tokens += now.Sub(b.last).Seconds() * rl.rate
	if b.tokens > rl.burst {
		b.tokens = rl.burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func (rl *rateLimiter) sweepLoop() {
	t := time.NewTicker(5 * time.Minute)
	for range t.C {
		now := time.Now()
		idle := time.Duration(rl.burst/rl.rate*float64(time.Second)) + time.Minute
		rl.mu.Lock()
		for ip, b := range rl.buckets {
			if now.Sub(b.last) > idle {
				delete(rl.buckets, ip)
			}
		}
		rl.mu.Unlock()
	}
}

func (rl *rateLimiter) wrap(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !rl.allow(clientIP(r)) {
			writeError(w, http.StatusTooManyRequests, "rate limit exceeded, slow down")
			return
		}
		next(w, r)
	}
}

// clientIP prefers the trusted proxy's X-Forwarded-For (Caddy sits in front),
// falling back to the connection's RemoteAddr.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			xff = xff[:i]
		}
		return strings.TrimSpace(xff)
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
