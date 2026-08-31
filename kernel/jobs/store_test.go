// SPDX-License-Identifier: AGPL-3.0-only

package jobs

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

func TestEnqueueClaimComplete(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			now := time.Now().UTC()

			id, err := Enqueue(ctx, db, tenant, "test.kind", []byte(`{"n":1}`), now, "")
			if err != nil {
				t.Fatalf("Enqueue: %v", err)
			}

			job, err := Claim(ctx, db, tenant, "worker-1", now)
			if err != nil {
				t.Fatalf("Claim: %v", err)
			}
			if job == nil {
				t.Fatal("Claim returned no job; one was due")
			}
			if job.ID != id || job.Kind != "test.kind" || string(job.Payload) != `{"n":1}` {
				t.Fatalf("Claim returned %+v, want the enqueued job", job)
			}
			if job.Attempts != 1 {
				t.Fatalf("Attempts = %d after one claim, want 1", job.Attempts)
			}

			// While the lease is held, nobody else gets it.
			again, err := Claim(ctx, db, tenant, "worker-2", now)
			if err != nil {
				t.Fatalf("Claim (second worker): %v", err)
			}
			if again != nil {
				t.Fatalf("a leased job was claimed twice: %+v", again)
			}

			if err := Complete(ctx, db, tenant, id); err != nil {
				t.Fatalf("Complete: %v", err)
			}
			done, err := Claim(ctx, db, tenant, "worker-1", now.Add(time.Hour))
			if err != nil {
				t.Fatalf("Claim (after complete): %v", err)
			}
			if done != nil {
				t.Fatalf("a completed job was claimed again: %+v", done)
			}
		})
	}
}

// A job scheduled for later is not due yet. Without this the queue is a
// stack that ignores run_at, and every scheduled trigger fires immediately.
func TestClaimRespectsRunAt(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			now := time.Now().UTC()

			if _, err := Enqueue(ctx, db, tenant, "later", nil, now.Add(time.Hour), ""); err != nil {
				t.Fatalf("Enqueue: %v", err)
			}
			job, err := Claim(ctx, db, tenant, "w", now)
			if err != nil {
				t.Fatalf("Claim: %v", err)
			}
			if job != nil {
				t.Fatalf("claimed a job scheduled an hour out: %+v", job)
			}
			// Non-vacuity: the same job is claimable once its time arrives, so
			// the refusal above is about run_at and not about the row being
			// absent or malformed.
			job, err = Claim(ctx, db, tenant, "w", now.Add(2*time.Hour))
			if err != nil {
				t.Fatalf("Claim (later): %v", err)
			}
			if job == nil {
				t.Fatal("the job was never claimable, even after its run_at")
			}
		})
	}
}

// The lease is what makes a crashed worker survivable: nothing observes the
// crash, the lease simply expires and the job becomes claimable again.
func TestExpiredLeaseIsReclaimable(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			now := time.Now().UTC()

			if _, err := Enqueue(ctx, db, tenant, "orphan", nil, now, ""); err != nil {
				t.Fatalf("Enqueue: %v", err)
			}
			first, err := Claim(ctx, db, tenant, "doomed-worker", now)
			if err != nil || first == nil {
				t.Fatalf("Claim: job = %v, err = %v", first, err)
			}
			// …and that worker dies here, without Complete or Fail.

			after := now.Add(LeaseDuration + time.Minute)
			second, err := Claim(ctx, db, tenant, "survivor", after)
			if err != nil {
				t.Fatalf("Claim (after lease): %v", err)
			}
			if second == nil {
				t.Fatal("a job whose worker died was never reclaimed — the lease does not expire")
			}
			if second.ID != first.ID {
				t.Fatalf("reclaimed %s, want the orphaned %s", second.ID, first.ID)
			}
			if second.Attempts != 2 {
				t.Fatalf("Attempts = %d on reclaim, want 2 — a redelivery must count", second.Attempts)
			}
		})
	}
}

// Retries back off, and the attempt that exhausts them files a dead letter and
// takes the job out of the queue. A head that retries forever blocks everything
// behind it — the stall INV-S4 counts as a silent drop.
func TestFailRetriesThenDeadLetters(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			now := time.Now().UTC()

			id, err := Enqueue(ctx, db, tenant, "flaky", []byte("payload"), now, "")
			if err != nil {
				t.Fatalf("Enqueue: %v", err)
			}

			for attempt := 1; attempt <= MaxAttempts; attempt++ {
				job, err := Claim(ctx, db, tenant, "w", now.Add(time.Duration(attempt)*time.Hour))
				if err != nil {
					t.Fatalf("Claim (attempt %d): %v", attempt, err)
				}
				if job == nil {
					t.Fatalf("attempt %d: no job to claim; the retry was not rescheduled", attempt)
				}
				if job.Attempts != attempt {
					t.Fatalf("attempt %d: Attempts = %d", attempt, job.Attempts)
				}
				if err := Fail(ctx, db, tenant, id, fmt.Errorf("attempt %d exploded", attempt)); err != nil {
					t.Fatalf("Fail (attempt %d): %v", attempt, err)
				}
			}

			// Out of attempts: gone from the queue…
			gone, err := Claim(ctx, db, tenant, "w", now.Add(24*time.Hour))
			if err != nil {
				t.Fatalf("Claim (after dead letter): %v", err)
			}
			if gone != nil {
				t.Fatalf("a dead-lettered job is still queued: %+v", gone)
			}
			// …and filed where a person can see it, with the cause.
			letters, err := DeadLetters(ctx, db, tenant, "")
			if err != nil {
				t.Fatalf("DeadLetters: %v", err)
			}
			if len(letters) != 1 {
				t.Fatalf("got %d dead letters, want 1", len(letters))
			}
			if letters[0].JobID != id || letters[0].Kind != "flaky" || letters[0].Attempts != MaxAttempts {
				t.Fatalf("dead letter = %+v, want the failed job", letters[0])
			}
			if letters[0].Error == "" || letters[0].Payload != "payload" {
				t.Fatalf("dead letter lost the cause or the payload: %+v", letters[0])
			}
		})
	}
}

func TestBackoffGrowsAndIsCapped(t *testing.T) {
	prev := time.Duration(0)
	for attempts := 1; attempts <= 12; attempts++ {
		d := backoff(attempts)
		if d < prev {
			t.Fatalf("backoff(%d) = %s, shorter than backoff(%d) = %s", attempts, d, attempts-1, prev)
		}
		if d > retryMax {
			t.Fatalf("backoff(%d) = %s, over the %s cap", attempts, d, retryMax)
		}
		prev = d
	}
	if backoff(1) != retryBase {
		t.Fatalf("backoff(1) = %s, want %s", backoff(1), retryBase)
	}
	if backoff(12) != retryMax {
		t.Fatalf("backoff(12) = %s, want the cap %s", backoff(12), retryMax)
	}
}

// Deduplication is the producer's idempotency seam: enqueue_job is reachable
// from an async hook, and async delivery is at-least-once by design (WP-3.1b),
// so a redelivered hook must find its own earlier job rather than make a
// second one.
func TestEnqueueDeduplicates(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			now := time.Now().UTC()

			first, err := Enqueue(ctx, db, tenant, "k", nil, now, "invoice-42")
			if err != nil {
				t.Fatalf("Enqueue: %v", err)
			}
			second, err := Enqueue(ctx, db, tenant, "k", nil, now, "invoice-42")
			if err != nil {
				t.Fatalf("Enqueue (duplicate): %v", err)
			}
			if second != first {
				t.Fatalf("duplicate enqueue made a second job (%s vs %s)", second, first)
			}
			// A different key is a different job, or "deduplication" would just
			// be a queue that holds one item.
			other, err := Enqueue(ctx, db, tenant, "k", nil, now, "invoice-43")
			if err != nil {
				t.Fatalf("Enqueue (other key): %v", err)
			}
			if other == first {
				t.Fatal("two different dedupe keys collapsed into one job")
			}
			// And no key means no deduplication.
			a, _ := Enqueue(ctx, db, tenant, "k", nil, now, "")
			b, _ := Enqueue(ctx, db, tenant, "k", nil, now, "")
			if a == b {
				t.Fatal("two keyless enqueues were deduplicated against each other")
			}
		})
	}
}

// The claim is the one piece of concurrency control in the queue, so it is
// tested by contention rather than by reading the SQL: N workers race for M
// jobs and every job must be claimed exactly once.
func TestConcurrentClaimsNeverDoubleDeliver(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			now := time.Now().UTC()

			const jobCount, workers = 20, 8
			for i := 0; i < jobCount; i++ {
				if _, err := Enqueue(ctx, db, tenant, "race", []byte(fmt.Sprint(i)), now, ""); err != nil {
					t.Fatalf("Enqueue: %v", err)
				}
			}

			var mu sync.Mutex
			seen := map[string]int{}
			var wg sync.WaitGroup
			for w := 0; w < workers; w++ {
				wg.Add(1)
				go func(w int) {
					defer wg.Done()
					for {
						job, err := Claim(ctx, db, tenant, fmt.Sprintf("w%d", w), now)
						if err != nil {
							t.Errorf("Claim: %v", err)
							return
						}
						if job == nil {
							return
						}
						mu.Lock()
						seen[job.ID]++
						mu.Unlock()
						if err := Complete(ctx, db, tenant, job.ID); err != nil {
							t.Errorf("Complete: %v", err)
							return
						}
					}
				}(w)
			}
			wg.Wait()

			if len(seen) != jobCount {
				t.Fatalf("claimed %d distinct jobs, want %d — work was lost", len(seen), jobCount)
			}
			for id, n := range seen {
				if n != 1 {
					t.Fatalf("job %s was claimed %d times; the compare-and-set does not exclude", id, n)
				}
			}
		})
	}
}

func TestFailOnUnknownJob(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			tenant := mustCreateTenant(t, db)
			if err := Fail(context.Background(), db, tenant, "nope", errors.New("x")); !errors.Is(err, ErrNotFound) {
				t.Fatalf("Fail on an unknown job: err = %v, want ErrNotFound", err)
			}
		})
	}
}

// One tenant's queue is invisible to another's. RLS is the backstop on
// Postgres; the tenant predicate is the whole of it on SQLite (INV-T1).
func TestQueueIsTenantScoped(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			a, b := mustCreateTenant(t, db), mustCreateTenant(t, db)
			now := time.Now().UTC()

			if _, err := Enqueue(ctx, db, a, "theirs", nil, now, ""); err != nil {
				t.Fatalf("Enqueue: %v", err)
			}
			job, err := Claim(ctx, db, b, "w", now)
			if err != nil {
				t.Fatalf("Claim (other tenant): %v", err)
			}
			if job != nil {
				t.Fatalf("tenant %s claimed tenant %s's job: %+v", b, a, job)
			}
			// Non-vacuity: the owning tenant can claim it.
			job, err = Claim(ctx, db, a, "w", now)
			if err != nil || job == nil {
				t.Fatalf("the owning tenant could not claim its own job: job = %v, err = %v", job, err)
			}
		})
	}
}

var _ = storage.Postgres // keep the storage import honest across build tags
var _ tenancy.ID
