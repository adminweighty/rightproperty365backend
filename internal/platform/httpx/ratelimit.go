package httpx

import (
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// RateLimiter is a per-key token bucket held in process memory.
//
// IMPORTANT: this limiter is per process. As soon as the API runs more than
// one instance (autoscaling, blue/green) each instance keeps its own buckets
// and the effective limit multiplies. Replace it with a shared limiter
// (e.g. Redis/Valkey or the edge/WAF) before scaling out. It also keys on the
// connection's peer IP (see ClientIP), which is only meaningful when the API
// is not behind an untrusted-header-forwarding proxy.
type RateLimiter struct {
	mu        sync.Mutex
	buckets   map[string]*bucket
	rate      float64 // tokens per second
	burst     float64
	idleTTL   time.Duration
	lastSweep time.Time
	now       func() time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

// NewRateLimiter allows bursts of up to burst requests per key, refilled at
// perMinute tokens per minute.
func NewRateLimiter(perMinute, burst int) *RateLimiter {
	return &RateLimiter{
		buckets: make(map[string]*bucket),
		rate:    float64(perMinute) / 60,
		burst:   float64(burst),
		idleTTL: 10 * time.Minute,
		now:     time.Now,
	}
}

// Allow consumes one token for key. When the bucket is empty it returns false
// and how long until the next token is available.
func (l *RateLimiter) Allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	l.sweep(now)

	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens = math.Min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now

	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	wait := time.Duration((1 - b.tokens) / l.rate * float64(time.Second))
	return false, wait
}

// sweep drops idle buckets so the map cannot grow without bound.
func (l *RateLimiter) sweep(now time.Time) {
	if now.Sub(l.lastSweep) < time.Minute {
		return
	}
	l.lastSweep = now
	for k, b := range l.buckets {
		if now.Sub(b.last) > l.idleTTL {
			delete(l.buckets, k)
		}
	}
}

// Middleware rejects requests over the limit with 429 and Retry-After.
func (l *RateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ok, wait := l.Allow(ClientIP(r))
		if !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
			WriteError(w, r, http.StatusTooManyRequests, CodeRateLimited, "Too many requests. Please wait a moment and try again.", nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}
