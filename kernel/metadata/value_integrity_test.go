//go:build integrity

package metadata

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/iamdoubz/lasterp/kernel/authz"
	"github.com/iamdoubz/lasterp/kernel/identity"
	"github.com/iamdoubz/lasterp/kernel/idgen"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// INV-T5: every stored field value conforms to its object's effective schema —
// declared type and declared option set; no write path stores a value outside
// it. INV-T3 (option ceiling) and INV-T4 (attributable, and the audit trail
// agrees with the row) are exercised here too.
//
// Everything runs on Postgres AND SQLite: validation that behaves differently
// per dialect is not validation, and normalization exists precisely because the
// two drivers disagree about what to do with a JSON float in an INT column.

const ledgerAccountYAML = `
object: LedgerProbe
module: probe
persistence: crud
fields:
  - {name: code, type: text, required: true}
  - {name: type, type: enum, required: true, options: [asset, liability, equity, income, expense]}
  - {name: seq, type: int}
permissions:
  read: [probe.viewer]
  create: [probe.viewer]
  update: [probe.viewer]
  delete: [probe.viewer]
`

// probeCRUD builds the probe object's engine, optionally through overlays.
func probeCRUD(t *testing.T, db *storage.DB, overlays ...Overlay) *CRUD {
	t.Helper()
	core, err := ParseObject([]byte(ledgerAccountYAML))
	if err != nil {
		t.Fatalf("ParseObject: %v", err)
	}
	eff, err := Merge(core, overlays...)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if err := ApplyDDL(context.Background(), db, eff, 1); err != nil &&
		!errors.Is(err, ErrSchemaVersionConflict) {
		t.Fatalf("ApplyDDL: %v", err)
	}
	crud, err := NewCRUD(eff)
	if err != nil {
		t.Fatalf("NewCRUD: %v", err)
	}
	return crud
}

func probeActor(t *testing.T, db *storage.DB, tenant tenancy.ID) context.Context {
	t.Helper()
	ctx := context.Background()
	hash, err := identity.HashPassword("s3cret!")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	user, err := identity.CreateUser(ctx, db, tenant, idgen.New()+"@example.com", hash)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	role, err := authz.CreateRole(ctx, db, tenant, "probe-"+idgen.New(), false)
	if err != nil {
		t.Fatalf("CreateRole: %v", err)
	}
	for _, action := range []string{"create", "read", "update", "delete"} {
		if err := authz.GrantPermission(ctx, db, tenant, role, "LedgerProbe", action, ""); err != nil {
			t.Fatalf("GrantPermission(%s): %v", action, err)
		}
	}
	if err := authz.AssignRole(ctx, db, tenant, user.ID, role); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}
	return authz.WithActor(ctx, authz.Actor{TenantID: tenant, UserID: user.ID})
}

// TestOutOfSetEnumWriteIsRefused is WP-1.11's AC 1 at the engine: the finding
// (`POST /api/v1/account {"type":"banana"}` accepted into the chart of
// accounts) reduced to its cause, on both dialects, on create and on update.
func TestOutOfSetEnumWriteIsRefused(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			tenant := mustCreateTenant(t, db)
			crud := probeCRUD(t, db)
			ctx := probeActor(t, db, tenant)

			if _, err := crud.Create(ctx, db, tenant, Record{"code": "1000", "type": "banana"}); !errors.Is(err, ErrValidation) {
				t.Fatalf("Create with out-of-set enum = %v, want ErrValidation", err)
			}

			created, err := crud.Create(ctx, db, tenant, Record{"code": "1000", "type": "asset"})
			if err != nil {
				t.Fatalf("Create with in-set enum: %v", err)
			}
			id := created["id"].(string)

			// Until WP-1.11, Update ran no validation at all — this is the
			// half of the hole nothing had recorded.
			if _, err := crud.Update(ctx, db, tenant, id, Record{"type": "banana"}); !errors.Is(err, ErrValidation) {
				t.Fatalf("Update to out-of-set enum = %v, want ErrValidation", err)
			}

			got, err := crud.Get(ctx, db, tenant, id)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got["type"] != "asset" {
				t.Fatalf("stored type = %v, want asset (the refused update must not have landed)", got["type"])
			}
		})
	}
}

// TestUpdateRefusesNullingRequiredField: required-ness was enforced on create
// and silently unenforced on update, because Update never called validate.
func TestUpdateRefusesNullingRequiredField(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			tenant := mustCreateTenant(t, db)
			crud := probeCRUD(t, db)
			ctx := probeActor(t, db, tenant)

			created, err := crud.Create(ctx, db, tenant, Record{"code": "1000", "type": "asset"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			id := created["id"].(string)

			for _, empty := range []any{nil, ""} {
				if _, err := crud.Update(ctx, db, tenant, id, Record{"code": empty}); !errors.Is(err, ErrValidation) {
					t.Fatalf("Update nulling required field with %#v = %v, want ErrValidation", empty, err)
				}
			}
		})
	}
}

// TestIntNormalizesJSONFloat covers the adapter divergence normalization
// exists for: a JSON body's integer arrives as float64, and handing that to
// the driver for an INT column is absorbed by SQLite's type affinity and not
// by Postgres. Both dialects must read back an int64.
func TestIntNormalizesJSONFloat(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			tenant := mustCreateTenant(t, db)
			crud := probeCRUD(t, db)
			ctx := probeActor(t, db, tenant)

			created, err := crud.Create(ctx, db, tenant, Record{
				"code": "1000", "type": "asset", "seq": float64(42),
			})
			if err != nil {
				t.Fatalf("Create with JSON-shaped int: %v", err)
			}
			if created["seq"] != int64(42) {
				t.Fatalf("Create returned seq %#v, want int64(42)", created["seq"])
			}

			got, err := crud.Get(ctx, db, tenant, created["id"].(string))
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got["seq"] != int64(42) {
				t.Fatalf("stored seq = %#v, want int64(42)", got["seq"])
			}

			if _, err := crud.Create(ctx, db, tenant, Record{
				"code": "1001", "type": "asset", "seq": 42.5,
			}); !errors.Is(err, ErrValidation) {
				t.Fatalf("Create with fractional int = %v, want ErrValidation", err)
			}
		})
	}
}

// TestLegacyOutOfSetRowStaysEditable is WP-1.11's written answer to "what
// happens to rows already holding out-of-set values": nothing rewrites them,
// reads still work, and an update that does not touch the offending field
// still succeeds. Stranding those rows would be a worse outcome than the
// values themselves — the fix is one write of a declared value.
func TestLegacyOutOfSetRowStaysEditable(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			tenant := mustCreateTenant(t, db)
			crud := probeCRUD(t, db)
			ctx := probeActor(t, db, tenant)

			// A row as it would have been written before validation existed.
			id := idgen.New()
			now := time.Now().UTC()
			err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, db.Rebind(
					`INSERT INTO `+TableName("LedgerProbe")+
						` (id, tenant_id, code, type, seq, custom_fields, created_at, updated_at)
					  VALUES (?, ?, ?, ?, ?, '{}', ?, ?)`),
					id, string(tenant), "9999", "banana", nil, now, now)
				return err
			})
			if err != nil {
				t.Fatalf("seed legacy row: %v", err)
			}

			// Reads must not validate: a row you cannot read is a row you
			// cannot fix.
			got, err := crud.Get(ctx, db, tenant, id)
			if err != nil {
				t.Fatalf("Get legacy row: %v", err)
			}
			if got["type"] != "banana" {
				t.Fatalf("legacy value = %v, want it returned untouched", got["type"])
			}
			if _, err := crud.List(ctx, db, tenant); err != nil {
				t.Fatalf("List with a legacy row present: %v", err)
			}

			// An update touching another field must succeed despite the row
			// holding an out-of-set value elsewhere.
			if _, err := crud.Update(ctx, db, tenant, id, Record{"code": "8888"}); err != nil {
				t.Fatalf("Update of an untouched-enum legacy row: %v", err)
			}

			// And setting the field to a declared value is the fix.
			if _, err := crud.Update(ctx, db, tenant, id, Record{"type": "asset"}); err != nil {
				t.Fatalf("Update fixing the legacy value: %v", err)
			}
			fixed, err := crud.Get(ctx, db, tenant, id)
			if err != nil {
				t.Fatalf("Get after fix: %v", err)
			}
			if fixed["type"] != "asset" {
				t.Fatalf("after fix type = %v, want asset", fixed["type"])
			}
		})
	}
}

// TestAuditRecordsNormalizedValue — INV-T4. The audit trail must record what
// was stored, not what was submitted: normalization means those can differ
// (float64(7) is stored as int64(7); "usd" as "USD"), and a trail that
// disagrees with the row is not a trail.
func TestAuditRecordsNormalizedValue(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			tenant := mustCreateTenant(t, db)
			crud := probeCRUD(t, db)
			ctx := probeActor(t, db, tenant)

			created, err := crud.Create(ctx, db, tenant, Record{"code": "1000", "type": "asset"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			id := created["id"].(string)

			if _, err := crud.Update(ctx, db, tenant, id, Record{"seq": float64(7)}); err != nil {
				t.Fatalf("Update: %v", err)
			}

			var changes string
			err = tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
				return tx.QueryRowContext(ctx, db.Rebind(
					`SELECT changes FROM audit_log WHERE tenant_id = ? AND record_id = ? AND action = 'update'`),
					string(tenant), id).Scan(&changes)
			})
			if err != nil {
				t.Fatalf("read audit row: %v", err)
			}

			var diff map[string]map[string]any
			if err := json.Unmarshal([]byte(changes), &diff); err != nil {
				t.Fatalf("unmarshal audit changes: %v", err)
			}
			// JSON round-trips numbers as float64, so the assertion is that
			// the recorded value is the integer 7 rather than, say, a string.
			if got := diff["seq"]["new"]; got != float64(7) {
				t.Fatalf("audit recorded seq = %#v, want the normalized 7", got)
			}
		})
	}
}

// TestNarrowedOptionBindsTheWritePath — INV-T3. The option ceiling is only
// meaningful if it reaches writes: a value that is in the core set but outside
// a tenant overlay's narrowed set must be refused, on both dialects. This is
// the data-side counterpart of ErrPermissionFloorLowered.
func TestNarrowedOptionBindsTheWritePath(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			tenant := mustCreateTenant(t, db)
			ctx := probeActor(t, db, tenant)

			// Same physical table; only this tenant's effective schema is
			// narrowed. That is exactly why the ceiling cannot be a CHECK
			// constraint — the table is shared by every tenant.
			narrowed := probeCRUD(t, db, Overlay{
				Layer:         "tenant",
				NarrowOptions: map[string][]string{"type": {"asset", "liability"}},
			})

			if _, err := narrowed.Create(ctx, db, tenant, Record{"code": "4000", "type": "income"}); !errors.Is(err, ErrValidation) {
				t.Fatalf("Create of a core-legal but tenant-narrowed value = %v, want ErrValidation", err)
			}
			if _, err := narrowed.Create(ctx, db, tenant, Record{"code": "1000", "type": "asset"}); err != nil {
				t.Fatalf("Create of a narrowed-legal value: %v", err)
			}

			// The unnarrowed schema still accepts the full core set, proving
			// the restriction rode on the overlay rather than the table.
			if _, err := probeCRUD(t, db).Create(ctx, db, tenant, Record{"code": "4001", "type": "income"}); err != nil {
				t.Fatalf("Create against the unnarrowed schema: %v", err)
			}
		})
	}
}
