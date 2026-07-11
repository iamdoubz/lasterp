// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"math"
	"sync"
	"time"
)

// RateLimit configures the gateway's per-caller token bucket (ADR-009:
// rate limits per token + per tenant). A zero RequestsPerSecond disables
// limiting entirely.
type RateLimit struct {
	RequestsPerSecond float64
	Burst             float64
}

// rateLimiter is a hand-rolled token-bucket limiter, one bucket per key
// (we key per tenant+actor). Deliberately dependency-free: golang.org/x/
// time/rate would need an ADR for a new runtime dependency (CLAUDE.md), and
// a token bucket is a few lines. Buckets are kept in memory for the process
// lifetime; a solo/kernel gateway has a bounded set of active callers, and
// eviction of idle buckets is a future refinement, not a v1 need.
type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rate    float64 // tokens per second
	burst   float64
	now     func() time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newRateLimiter(rl RateLimit, now func() time.Time) *rateLimiter {
	return &rateLimiter{
		buckets: make(map[string]*bucket),
		rate:    rl.RequestsPerSecond,
		burst:   rl.Burst,
		now:     now,
	}
}

// decision is what allow reports back so the caller can set RateLimit-*
// headers consistently on both the allowed and the 429 path.
type decision struct {
	limited   bool
	limit     int
	remaining int
	resetSecs int
}

// allow consumes one token for key, refilling first. When limiting is
// disabled (rate <= 0) it always allows and reports limited=false with a
// zero limit so the caller omits the headers.
func (l *rateLimiter) allow(key string) decision {
	if l.rate <= 0 {
		return decision{limited: false, limit: 0}
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens = math.Min(l.burst, b.tokens+elapsed*l.rate)
		b.last = now
	}

	d := decision{limit: int(l.burst)}
	if b.tokens >= 1 {
		b.tokens--
		d.remaining = int(b.tokens)
		d.resetSecs = int(math.Ceil((l.burst - b.tokens) / l.rate))
		return d
	}
	// No token available: report how long until one refills.
	d.limited = true
	d.remaining = 0
	d.resetSecs = int(math.Ceil((1 - b.tokens) / l.rate))
	return d
}
