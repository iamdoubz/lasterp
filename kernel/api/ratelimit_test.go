// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"strconv"
	"testing"
	"time"
)

// TestRateLimiterBucketMapIsBounded: a caller rotating through unbounded
// distinct keys must not grow the bucket map without limit (the memory-growth
// DoS phase-0-review flagged, WP-0.10). Eviction targets the oldest (idle)
// buckets, so an actively-used key — touched every iteration — survives.
func TestRateLimiterBucketMapIsBounded(t *testing.T) {
	// Counter clock: each call advances time, so bucket last-seen times are
	// strictly ordered and eviction is deterministic.
	var tick int64
	clock := func() time.Time { tick++; return time.Unix(tick, 0) }
	l := newRateLimiter(RateLimit{RequestsPerSecond: 1000, Burst: 1000}, clock)

	const active = "active-caller"
	for i := 0; i < maxBuckets*2; i++ {
		l.allow(active)                     // newest last-seen every iteration
		l.allow("flood-" + strconv.Itoa(i)) // unique, never touched again
	}

	l.mu.Lock()
	n := len(l.buckets)
	_, activePresent := l.buckets[active]
	l.mu.Unlock()

	if n > maxBuckets {
		t.Fatalf("bucket map grew to %d, want <= %d (unbounded-map DoS)", n, maxBuckets)
	}
	if !activePresent {
		t.Fatal("active caller's bucket was evicted; eviction must target idle buckets, not active ones")
	}
}
