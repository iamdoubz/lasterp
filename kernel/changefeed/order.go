// SPDX-License-Identifier: AGPL-3.0-only

package changefeed

import (
	"context"
	"database/sql"
	"fmt"
	"hash/fnv"

	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// serializeAppend makes the feed's id order equal its commit order, which is
// the property every cursored reader depends on and the one BIGSERIAL does not
// give (INV-S5, WP-2.1-decisions.md §2).
//
// Without it: a transaction takes id 5 at INSERT and commits after one holding
// id 6. A reader sees 6, advances its cursor to 6, and never sees 5 again. The
// change is durable, acknowledged, and invisible to every replica forever.
//
// The lock is taken immediately before the feed INSERT and released by commit,
// so the window is the commit itself — the transaction's real work has already
// happened by the time an append runs. It is keyed per TENANT because docs/04
// specifies a totally-ordered *per-tenant* log: two tenants writing at once
// have no ordering relationship to preserve and must not queue behind each
// other.
//
// ponytail: one feed append at a time per tenant. If a single tenant's write
// throughput ever hits that ceiling, the upgrade path is a per-scope lock
// (scope keys already exist, WP-2.4 fills them in) or a Postgres-only WAL tail
// behind the reader interface — not a weaker ordering guarantee.
//
// SQLite needs nothing: it holds a write lock for the whole write transaction,
// so only one writer can hold an unassigned id at a time and id order is
// already commit order.
//
// Why not a snapshot horizon (xmin): a row's xmin is its transaction's id,
// assigned at that transaction's FIRST write, while its feed id is assigned at
// the append. Appends run after the write they describe, so the two orders
// diverge — a still-running xid 105 can hold feed id 5 while a committed xid
// 100 holds feed id 9, and "xmin < pg_snapshot_xmin" would call 9 stable and
// strand 5. The ordering has to come from the sequence, not from xids.
func serializeAppend(ctx context.Context, tx *sql.Tx, db *storage.DB, tenant tenancy.ID) error {
	if db.Dialect != storage.Postgres {
		return nil
	}
	// pg_advisory_xact_lock releases at commit or rollback — there is no
	// unlock path to leak, and a crashed backend drops it with its session.
	if _, err := tx.ExecContext(ctx, db.Rebind(`SELECT pg_advisory_xact_lock(?)`), advisoryKey(tenant)); err != nil {
		return fmt.Errorf("changefeed: serialize append: %w", err)
	}
	return nil
}

// advisoryKey maps a tenant id onto the int64 keyspace pg_advisory_xact_lock
// takes. A collision between two tenants costs throughput (they queue behind
// each other) and nothing else — never correctness — so a plain 64-bit hash is
// sufficient and no collision handling is needed.
func advisoryKey(tenant tenancy.ID) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(tenant))
	return int64(h.Sum64())
}
