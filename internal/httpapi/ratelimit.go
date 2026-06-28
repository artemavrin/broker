package httpapi

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// rateLimiter is a minimal per-key fixed-window limiter used to throttle
// /auth/token attempts per client IP. It is intentionally simple: in-memory,
// best-effort, and self-pruning. A limit of zero disables it.
type rateLimiter struct {
	limit  int
	window time.Duration

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	count       int
	windowStart time.Time
}

func newRateLimiter(perMinute int) *rateLimiter {
	return &rateLimiter{
		limit:   perMinute,
		window:  time.Minute,
		buckets: make(map[string]*bucket),
	}
}

// allow reports whether a request from key may proceed, accounting it against
// the current window.
func (rl *rateLimiter) allow(key string) bool {
	if rl.limit <= 0 {
		return true
	}
	now := time.Now()

	rl.mu.Lock()
	defer rl.mu.Unlock()

	b := rl.buckets[key]
	if b == nil || now.Sub(b.windowStart) >= rl.window {
		rl.buckets[key] = &bucket{count: 1, windowStart: now}
		rl.maybePrune(now)
		return true
	}
	if b.count >= rl.limit {
		return false
	}
	b.count++
	return true
}

// maybePrune drops expired buckets opportunistically to bound memory. Caller
// must hold the lock.
func (rl *rateLimiter) maybePrune(now time.Time) {
	if len(rl.buckets) < 1024 {
		return
	}
	for k, b := range rl.buckets {
		if now.Sub(b.windowStart) >= rl.window {
			delete(rl.buckets, k)
		}
	}
}

// clientIP extracts the remote IP, ignoring the port. Proxy headers are not
// trusted by default.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
