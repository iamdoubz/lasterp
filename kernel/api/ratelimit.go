// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"math"
	"slices"
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

// maxBuckets caps the per-key bucket map so a caller rotating keys (distinct
// tenant/actor pairs) can't grow it without bound — the memory-growth DoS
// phase-0-review flagged. At capacity, a new key triggers evictLocked.
const maxBuckets = 10_000

// rateLimiter is a hand-rolled token-bucket limiter, one bucket per key
// (we key per tenant+actor). Deliberately dependency-free: golang.org/x/
// time/rate would need an ADR for a new runtime dependency (CLAUDE.md), and
// a token bucket is a few lines. The bucket map is bounded by maxBuckets
// with oldest-first eviction (see allow/evictLocked).
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
		if len(l.buckets) >= maxBuckets {
			l.evictLocked()
		}
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

// evictLocked drops roughly the oldest 1/8 of buckets (by last-seen time) to
// keep the map bounded. Evicting an idle bucket is behaviourally free: it
// would have refilled to a full burst before its owner's next request, so the
// owner gets an identical fresh bucket — active callers, touched recently,
// are the newest and survive. Caller holds l.mu; only invoked at capacity.
// ponytail: O(n log n) sort per eviction, amortized O(1)/insert since it fires
// once per ~maxBuckets/8 new keys; swap for an LRU list/heap only if caller
// churn ever outpaces the cap.
func (l *rateLimiter) evictLocked() {
	times := make([]time.Time, 0, len(l.buckets))
	for _, b := range l.buckets {
		times = append(times, b.last)
	}
	slices.SortFunc(times, func(a, b time.Time) int { return a.Compare(b) })
	cutoff := times[len(times)/8]
	for k, b := range l.buckets {
		if !b.last.After(cutoff) {
			delete(l.buckets, k)
		}
	}
}
