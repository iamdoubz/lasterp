// SPDX-License-Identifier: AGPL-3.0-only

package integrity

import (
	"context"
	"fmt"
	"regexp"

	"github.com/iamdoubz/lasterp/kernel/storage"
)

// identOK guards the one place a caller-supplied identifier is interpolated
// into DDL: role names cannot be passed as bind parameters in REVOKE, so we
// require a plain SQL identifier rather than trusting the caller. Table
// names come from the catalog (compile-time constants), so they are not
// re-validated here.
var identOK = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// EnforceAppendOnlyGrants completes the docs/19 §2 role-separation layer for
// the append-only tables (INV-E1 events, INV-T4 audit_log): it revokes
// UPDATE, DELETE and TRUNCATE from role so the tables are immutable by lack
// of grant — "impossible", not merely "forbidden by trigger". The
// per-table triggers (migrations 0012/0021) remain as defense in depth and
// as the sole guard for connections this revoke doesn't cover (the owner
// role, and SQLite).
//
// ownerDB must be a connection whose role owns / can administer grants on
// the tables (the migration/superuser role), NOT the app role being
// restricted. INSERT stays granted: the write pipeline (eventstore.Append,
// metadata's recordAudit) runs as the app role and must append. Blocking
// even direct INSERT — routing appends through SECURITY DEFINER pipeline
// functions — is the full docs/19 layer-3 form, deferred until the posting
// pipeline is modelled as DB functions (Phase 1+); a raw INSERT still cannot
// beat INV-E2's unique (tenant_id, stream_id, version) index or alter any
// existing row, so history stays immutable regardless.
//
// SQLite has no role system and solo mode is a single trusted process
// (ADR-005), so this is a no-op there; the append-only trigger is the whole
// enforcement on SQLite.
func EnforceAppendOnlyGrants(ctx context.Context, ownerDB *storage.DB, role string) error {
	if ownerDB.Dialect != storage.Postgres {
		return nil
	}
	if !identOK.MatchString(role) {
		return fmt.Errorf("integrity: refusing to build grant DDL for non-identifier role %q", role)
	}
	for _, tbl := range ProtectedTables() {
		// tbl is a catalog constant; role is validated above.
		stmt := fmt.Sprintf("REVOKE UPDATE, DELETE, TRUNCATE ON %s FROM %s", tbl, role)
		if _, err := ownerDB.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("integrity: revoke mutation on %s from %s: %w", tbl, role, err)
		}
	}
	return nil
}
