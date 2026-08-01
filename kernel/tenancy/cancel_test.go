// SPDX-License-Identifier: AGPL-3.0-only

package tenancy

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/iamdoubz/lasterp/kernel/storage"
)

// A cancelled request must be reported as a cancellation, not as a storage
// fault. database/sql rolls the transaction back from its own goroutine when the
// context is done, so Commit afterwards returns either the context error or a
// bare sql.ErrTxDone depending on which lands first — and the edge classifies
// those two very differently: one is a non-event, the other is an unclassified
// 500 with a log line implying the database is broken.
//
// That is exactly what happened once WP-1.10 stopped the reporting layer
// swallowing every error: an ordinary page navigation started logging a server
// fault. Both orderings are covered here so the fix cannot regress into working
// for only the fast one.
func TestCancelledRequestIsNotAStorageFault(t *testing.T) {
	tests := []struct {
		name string
		// settle waits (or does not) after cancelling, to steer which of the two
		// orderings the Commit hits.
		settle time.Duration
	}{
		{name: "commit races the rollback", settle: 0},
		{name: "rollback lands first", settle: 100 * time.Millisecond},
	}

	for name, db := range map[string]*storage.DB{"postgres": testPostgresDB(t)} {
		t.Run(name, func(t *testing.T) {
			for _, tc := range tests {
				t.Run(tc.name, func(t *testing.T) {
					ctx, cancel := context.WithCancel(context.Background())
					err := WithTenant(ctx, db, "cancel-test", func(context.Context, *sql.Tx) error {
						cancel()
						time.Sleep(tc.settle)
						return nil
					})

					if err == nil {
						t.Fatal("a cancelled request committed successfully")
					}
					if !errors.Is(err, context.Canceled) {
						t.Errorf("error = %v; want it to wrap context.Canceled so the edge "+
							"reports a cancellation rather than an unclassified 500", err)
					}
					if errors.Is(err, sql.ErrTxDone) {
						t.Errorf("error surfaced as sql.ErrTxDone: %v", err)
					}
				})
			}
		})
	}
}
