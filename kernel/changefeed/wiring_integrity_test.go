//go:build integrity

package changefeed

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// INV-T1: a tenant's feed never surfaces another tenant's changes. The feed is
// the surface whose entire job is handing data to a device that then holds it
// offline, so a leak here leaves the building.
func TestFeedIsTenantScoped(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenantA := mustCreateTenant(t, db)
			tenantB := mustCreateTenant(t, db)

			appendCommitted(t, db, tenantA, "a-1")
			appendCommitted(t, db, tenantB, "b-1")
			appendCommitted(t, db, tenantB, "b-2")

			feedA, err := Read(ctx, db, tenantA, 0, 100, nil)
			if err != nil {
				t.Fatalf("read A: %v", err)
			}
			if len(feedA) != 1 || feedA[0].RefID != "a-1" || feedA[0].TenantID != tenantA {
				t.Fatalf("tenant A's feed = %+v, want exactly its own one change", feedA)
			}

			feedB, err := Read(ctx, db, tenantB, 0, 100, nil)
			if err != nil {
				t.Fatalf("read B: %v", err)
			}
			if len(feedB) != 2 {
				t.Fatalf("tenant B's feed = %+v, want its own two changes", feedB)
			}
			for _, c := range feedB {
				if c.TenantID != tenantB {
					t.Fatalf("tenant B's feed carried a row belonging to %q", c.TenantID)
				}
			}
		})
	}
}

// INV-S5: a feed entry is atomic with the write it describes. A rolled-back
// write must leave nothing behind — a feed pointing at a row that never
// committed would send every replica to fetch a phantom.
func TestFeedAppendIsTransactional(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)

			// A transaction that appends and then fails leaves no trace.
			wantErr := errors.New("business rule said no")
			err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
				if err := Append(ctx, tx, db, testEntry(tenant, "rolled-back")); err != nil {
					return err
				}
				return wantErr
			})
			if !errors.Is(err, wantErr) {
				t.Fatalf("WithTenant err = %v, want %v", err, wantErr)
			}

			got, err := Read(ctx, db, tenant, 0, 100, nil)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if len(got) != 0 {
				t.Fatalf("rolled-back write left %d feed entr(ies): %+v", len(got), got)
			}

			// And the committed one after it still works — the failed attempt
			// did not wedge the ordering lock.
			appendCommitted(t, db, tenant, "committed")
			got, err = Read(ctx, db, tenant, 0, 100, nil)
			if err != nil {
				t.Fatalf("read after commit: %v", err)
			}
			if len(got) != 1 || got[0].RefID != "committed" {
				t.Fatalf("feed = %+v, want exactly the committed change", got)
			}
		})
	}
}

// INV-S5: the feed is append-only. A rewritable feed can lie to a replica that
// already consumed it — the client has no way to learn that cursor 7 now says
// something different from what it applied.
func TestFeedIsAppendOnly(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			appendCommitted(t, db, tenant, "immutable")

			for _, tc := range []struct {
				name, query string
			}{
				{"update", `UPDATE change_feed SET ref_id = 'tampered' WHERE tenant_id = ?`},
				{"delete", `DELETE FROM change_feed WHERE tenant_id = ?`},
			} {
				err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
					_, err := tx.ExecContext(ctx, db.Rebind(tc.query), string(tenant))
					return err
				})
				if err == nil {
					t.Fatalf("%s on change_feed succeeded; the feed must be append-only", tc.name)
				}
			}

			got, err := Read(ctx, db, tenant, 0, 100, nil)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if len(got) != 1 || got[0].RefID != "immutable" {
				t.Fatalf("feed = %+v, want the original entry untouched", got)
			}
		})
	}
}

// INV-S5: notifications are a bell, not the transport. Dropping every one of
// them costs latency and nothing else — a reader that polls still observes
// every committed change.
func TestNotifierDropDoesNotLoseChanges(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)

			// Subscribe, then never read the channel: after the first pending
			// notification every later one is dropped by design.
			n := NewInProcess()
			_, unsubscribe := n.Subscribe(tenant)
			defer unsubscribe()

			const total = 25
			for i := 0; i < total; i++ {
				appendCommitted(t, db, tenant, refFor(i))
				n.Notify(tenant, int64(i))
			}

			got, err := Read(ctx, db, tenant, 0, total+10, nil)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if len(got) != total {
				t.Fatalf("observed %d changes after dropping notifications, want %d", len(got), total)
			}
		})
	}
}

// A subscriber that is keeping up is woken with the current cursor.
func TestInProcessNotifierWakesSubscribers(t *testing.T) {
	n := NewInProcess()
	tenant := tenancy.ID("t1")
	ch, unsubscribe := n.Subscribe(tenant)
	defer unsubscribe()

	n.Notify(tenant, 42)
	select {
	case got := <-ch:
		if got != 42 {
			t.Fatalf("woken with cursor %d, want 42", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber was never woken")
	}

	// A different tenant's changes are not this subscriber's business.
	n.Notify(tenancy.ID("t2"), 99)
	select {
	case got := <-ch:
		t.Fatalf("woken by another tenant's change (cursor %d)", got)
	case <-time.After(100 * time.Millisecond):
	}
}

func refFor(i int) string {
	return "e" + string(rune('a'+i%26)) + string(rune('0'+i/26))
}
