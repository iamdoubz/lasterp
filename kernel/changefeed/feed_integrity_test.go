//go:build integrity

package changefeed

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// INV-S5: a cursor position cannot be allocated out of commit order.
//
// This is the test the whole WP exists for. change_feed.id is BIGSERIAL and a
// transaction takes its id when it INSERTs, not when it commits — so without
// the per-tenant ordering lock, a writer holding id 5 open lets a later writer
// take id 6 and commit first. A reader trusting "id > cursor" sees 6, advances
// to 6, and never sees 5 again. That change is durable, acknowledged to its
// writer, and invisible to every replica forever.
//
// The fix makes the interleaving unreachable rather than tolerated, so that is
// what this asserts: while A holds a position, B cannot take the next one. B's
// append blocks until A resolves, and the ids then match commit order.
//
// SQLite reaches the same state by a different route — one writer at a time,
// so the hole never opens — which is why the blocking half is Postgres-only
// while the ordering assertion runs on both.
func TestFeedNeverSkipsAnOpenTransaction(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)

			if db.Dialect == storage.Postgres {
				// Writer A takes a cursor position and holds it, uncommitted.
				txA, err := db.BeginTx(ctx, nil)
				if err != nil {
					t.Fatalf("begin A: %v", err)
				}
				defer func() { _ = txA.Rollback() }()
				ctxA, err := tenancy.SetContext(ctx, txA, db.Dialect, tenant)
				if err != nil {
					t.Fatalf("tenant context A: %v", err)
				}
				if err := Append(ctxA, txA, db, testEntry(tenant, "A")); err != nil {
					t.Fatalf("append A: %v", err)
				}

				// Writer B must NOT be able to take the next position while A
				// holds one. A short deadline is the probe: if the append
				// returns before it, B allocated an id out of commit order and
				// the hole is open.
				blocked, err := appendBlocks(ctx, db, tenant, "B", 750*time.Millisecond)
				if err != nil {
					t.Fatalf("probe B: %v", err)
				}
				if !blocked {
					t.Fatal("second writer allocated a cursor position while an earlier one was still in flight; " +
						"a reader consuming it would strand the earlier position permanently")
				}

				if err := txA.Commit(); err != nil {
					t.Fatalf("commit A: %v", err)
				}
			} else {
				appendCommitted(t, db, tenant, "A")
			}

			// With A resolved, B goes through, and the feed reads back in
			// commit order with nothing skipped.
			appendCommitted(t, db, tenant, "B")

			got, err := Read(ctx, db, tenant, 0, 100, nil)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if len(got) != 2 {
				t.Fatalf("got %d change(s), want 2: %+v", len(got), got)
			}
			if got[0].RefID != "A" || got[1].RefID != "B" {
				t.Fatalf("feed order = %s,%s; want A,B (commit order)", got[0].RefID, got[1].RefID)
			}
			if got[0].Cursor >= got[1].Cursor {
				t.Fatalf("cursors not increasing in commit order: %d then %d", got[0].Cursor, got[1].Cursor)
			}
		})
	}
}

// appendBlocks reports whether an append for tenant is still waiting on the
// ordering lock after d. A context deadline is what distinguishes "queued
// behind an earlier writer" (wanted) from "allocated a position anyway".
func appendBlocks(ctx context.Context, db *storage.DB, tenant tenancy.ID, ref string, d time.Duration) (bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	probeCtx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	txCtx, err := tenancy.SetContext(probeCtx, tx, db.Dialect, tenant)
	if err != nil {
		return false, err
	}

	err = Append(txCtx, tx, db, testEntry(tenant, ref))
	if errors.Is(err, context.DeadlineExceeded) {
		return true, nil
	}
	return false, err
}

// INV-S5 under concurrency: with many writers overlapping, a reader draining
// the feed to exhaustion observes every committed change exactly once.
//
// This is the same defect as the test above, reached the way production would
// reach it — nobody holds a transaction open on purpose, they just overlap.
func TestFeedObservesEveryCommittedChangeUnderConcurrency(t *testing.T) {
	const writers, perWriter = 16, 8

	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)

			var wg sync.WaitGroup
			errs := make(chan error, writers)
			for w := 0; w < writers; w++ {
				wg.Add(1)
				go func(w int) {
					defer wg.Done()
					for i := 0; i < perWriter; i++ {
						ref := fmt.Sprintf("w%d-%d", w, i)
						err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
							return Append(ctx, tx, db, testEntry(tenant, ref))
						})
						if err != nil {
							errs <- fmt.Errorf("append %s: %w", ref, err)
							return
						}
					}
				}(w)
			}

			// The reader drains WHILE the writers run — that is the whole
			// point. Draining only after they finish leaves no transaction in
			// flight, so a reader that steps over holes would pass and the
			// test would prove nothing.
			seen := map[string]int{}
			cursor := int64(0)
			drain := func() {
				for {
					page, err := Read(ctx, db, tenant, cursor, 10, nil)
					if err != nil {
						t.Errorf("read at cursor %d: %v", cursor, err)
						return
					}
					if len(page) == 0 {
						return
					}
					for _, c := range page {
						if c.Cursor <= cursor {
							t.Errorf("cursor went backwards: %d after %d", c.Cursor, cursor)
							return
						}
						cursor = c.Cursor
						seen[c.RefID]++
					}
				}
			}

			writing := make(chan struct{})
			go func() { wg.Wait(); close(writing) }()
			for done := false; !done; {
				select {
				case <-writing:
					done = true
				default:
				}
				drain()
			}
			// Final pass: anything committed after the last mid-flight drain.
			drain()

			close(errs)
			for err := range errs {
				t.Fatalf("writer failed: %v", err)
			}
			if t.Failed() {
				return
			}

			want := writers * perWriter
			if len(seen) != want {
				var missing []string
				for w := 0; w < writers; w++ {
					for i := 0; i < perWriter; i++ {
						ref := fmt.Sprintf("w%d-%d", w, i)
						if seen[ref] == 0 {
							missing = append(missing, ref)
						}
					}
				}
				t.Fatalf("observed %d of %d committed changes; %d skipped: %v",
					len(seen), want, len(missing), missing)
			}
			for ref, n := range seen {
				if n != 1 {
					t.Fatalf("change %s observed %d times, want exactly once", ref, n)
				}
			}
		})
	}
}

// INV-S5 (resume half) + the WP-2.1 AC: resuming from any cursor position
// reconstructs exactly the sequence an uninterrupted reader saw. A feed whose
// order depended on read timing would break every client that reconnects.
func TestFeedResumeFromAnyCursor(t *testing.T) {
	const total = 20

	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			for i := 0; i < total; i++ {
				appendCommitted(t, db, tenant, fmt.Sprintf("e%02d", i))
			}

			full, err := Read(ctx, db, tenant, 0, total+10, nil)
			if err != nil {
				t.Fatalf("read (full): %v", err)
			}
			if len(full) != total {
				t.Fatalf("full read got %d, want %d", len(full), total)
			}

			// Resuming at every position in the sequence must yield the same
			// tail the uninterrupted read produced.
			for split := 0; split < total; split++ {
				resumed, err := Read(ctx, db, tenant, full[split].Cursor, total+10, nil)
				if err != nil {
					t.Fatalf("read (resume at %d): %v", split, err)
				}
				want := full[split+1:]
				if len(resumed) != len(want) {
					t.Fatalf("resume at %d got %d changes, want %d", split, len(resumed), len(want))
				}
				for i := range want {
					if resumed[i].Cursor != want[i].Cursor || resumed[i].RefID != want[i].RefID {
						t.Fatalf("resume at %d diverged at %d: got (%d,%s), want (%d,%s)",
							split, i, resumed[i].Cursor, resumed[i].RefID, want[i].Cursor, want[i].RefID)
					}
				}
			}
		})
	}
}
