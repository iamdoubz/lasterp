//go:build integrity

// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/iamdoubz/lasterp/kernel/idgen"
	"github.com/iamdoubz/lasterp/kernel/metadata"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// INV-T5 over HTTP: the finding this WP closes was reported as an API-level
// defect ("POST /api/v1/account {"type":"banana"} is accepted into the chart of
// accounts today"), so it gets an API-level test. The engine tests in
// kernel/metadata prove the mechanism; these prove the product.

// TestPostAccountWithUnknownTypeIsRejected is the finding, verbatim.
func TestPostAccountWithUnknownTypeIsRejected(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)

			status, body, _ := e.post("/api/v1/account", map[string]any{
				"code": "1000", "name": "Nonsense", "type": "banana", "currency": "USD",
			})
			if status != http.StatusUnprocessableEntity {
				t.Fatalf("POST /api/v1/account with type=banana = %d, want 422; body=%s", status, body)
			}
			assertProblemJSON(t, e, body)

			// The same request with a declared value must still work, or the
			// fix has broken the product instead of the bug.
			if id := e.createAccount("1001", "Cash", "asset"); id == "" {
				t.Fatal("a well-formed account was refused")
			}

			// Contact.kind is the enum with no report bucket behind it: the
			// generic CRUD route bypassed contacts.CreateContact's check, so
			// nothing anywhere objected before this WP.
			status, body, _ = e.post("/api/v1/contact", map[string]any{
				"name": "Acme", "email": "ap@acme.example", "kind": "frenemy",
			})
			if status != http.StatusUnprocessableEntity {
				t.Fatalf("POST /api/v1/contact with kind=frenemy = %d, want 422; body=%s", status, body)
			}
		})
	}
}

// TestUpdateWithUnknownEnumIsRejected covers the half of the hole that had not
// been recorded anywhere: CRUD.Update never called validate at all, so a PUT
// was unvalidated even for required-ness.
func TestUpdateWithUnknownEnumIsRejected(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)
			id := e.createAccount("1000", "Cash", "asset")

			status, body, _ := e.call("PATCH", "/api/v1/account/"+id, e.token, idgen.New(),
				map[string]any{"type": "banana"})
			if status != http.StatusUnprocessableEntity {
				t.Fatalf("PATCH account type=banana = %d, want 422; body=%s", status, body)
			}

			// Nulling a required field on update was silently accepted before.
			status, body, _ = e.call("PATCH", "/api/v1/account/"+id, e.token, idgen.New(),
				map[string]any{"code": ""})
			if status != http.StatusUnprocessableEntity {
				t.Fatalf("PATCH account code=\"\" = %d, want 422; body=%s", status, body)
			}

			_, _, rec := e.call("GET", "/api/v1/account/"+id, e.token, "", nil)
			if rec["type"] != "asset" || rec["code"] != "1000" {
				t.Fatalf("refused updates leaked into the row: %v", rec)
			}
		})
	}
}

// TestUnauthorizedWriteIsNotAValidationOracle — INV-T2. Validation runs after
// the authorization decision, so a caller who may not write learns nothing
// about which values are legal. A 422 here would enumerate the chart of
// accounts' type vocabulary to an unauthorized principal.
func TestUnauthorizedWriteIsNotAValidationOracle(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)
			// A user with no Account grants at all.
			powerless := e.issueUser(t, map[string][]string{"Contact": {"read"}})

			status, body, _ := e.call("POST", "/api/v1/account", powerless, idgen.New(),
				map[string]any{"code": "1000", "name": "X", "type": "banana", "currency": "USD"})
			if status != http.StatusForbidden {
				t.Fatalf("unauthorized POST with an invalid body = %d, want 403 (never 422); body=%s", status, body)
			}
		})
	}
}

// TestEveryRegisteredEnumFieldDeclaresOptions is AC 1's "every enum field in a
// shipped module declares its options", asserted against what the running
// server actually registered rather than against a list someone maintains.
//
// Field.validate already makes a bare enum a parse error, so a module that
// forgot would fail in Register() and the server would not boot — this test
// makes that guarantee visible instead of implicit.
func TestEveryRegisteredEnumFieldDeclaresOptions(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			seed(t, db) // boots the server, which registers every module schema

			names := registeredNames(t, db)
			if len(names) == 0 {
				t.Fatal("no object schemas were registered")
			}

			enums := 0
			for _, object := range names {
				stored, err := metadata.LoadObjectSchema(context.Background(), db, "", metadata.LayerCore, object)
				if err != nil {
					t.Fatalf("load %s: %v", object, err)
				}
				parsed, err := metadata.ParseObject(stored.Definition)
				if err != nil {
					t.Fatalf("parse %s: %v", object, err)
				}
				for _, f := range parsed.Fields {
					if f.Type != metadata.FieldEnum {
						continue
					}
					enums++
					if len(f.Options) == 0 {
						t.Errorf("%s.%s is an enum with no declared options", object, f.Name)
					}
				}
			}
			// Five enum fields ship: Account.type, Period.status, Contact.kind,
			// Invoice.status, Receipt.status.
			if enums != 5 {
				t.Errorf("found %d enum fields, expected 5 — update this count deliberately, "+
					"and make sure the new one declares its options", enums)
			}
		})
	}
}

// TestDoctorReportsNonConformingRowsWithoutFailing is the detection half of
// WP-1.11's answer to "what happens to rows already holding out-of-set values".
// They are found and named — and the report does not change doctor's exit
// status, which means "role separation is not in effect" and is used as a
// deploy gate.
func TestDoctorReportsNonConformingRows(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)
			e.createAccount("1000", "Cash", "asset")

			clean, err := DiagnoseSchemaConformance(context.Background(), db)
			if err != nil {
				t.Fatalf("DiagnoseSchemaConformance: %v", err)
			}
			if len(clean.Findings) != 0 {
				t.Fatalf("a well-formed tenant reported findings: %+v", clean.Findings)
			}
			if clean.Checked == 0 {
				t.Fatal("scanned no fields — a report that checks nothing must not read as clean")
			}

			// A row as it was writable before validation existed.
			seedLegacyAccount(t, db, e.tenant, "9999", "banana")

			dirty, err := DiagnoseSchemaConformance(context.Background(), db)
			if err != nil {
				t.Fatalf("DiagnoseSchemaConformance: %v", err)
			}
			var found bool
			for _, f := range dirty.Findings {
				if f.Object == "Account" && f.Field == "type" && f.Value == "banana" {
					found = true
					if f.Count != 1 {
						t.Errorf("count = %d, want 1", f.Count)
					}
					if f.Tenant != string(e.tenant) {
						t.Errorf("tenant = %q, want %q", f.Tenant, e.tenant)
					}
				}
			}
			if !found {
				t.Fatalf("out-of-set row not reported; findings=%+v", dirty.Findings)
			}

			// Posture is about the connection, not the data: a legacy row must
			// not make a separated deployment look unseparated.
			posture, err := Diagnose(context.Background(), db)
			if err != nil {
				t.Fatalf("Diagnose: %v", err)
			}
			for _, f := range posture.Findings {
				if f == "" {
					continue
				}
				t.Logf("posture finding (unrelated to data): %s", f)
			}
			if db.Dialect != storage.Postgres && !posture.Separated {
				t.Error("data findings must not affect the role-separation verdict")
			}
		})
	}
}

// seedLegacyAccount inserts directly, bypassing the engine, to stand in for a
// row written before WP-1.11 existed.
func seedLegacyAccount(t *testing.T, db *storage.DB, tenant tenancy.ID, code, accountType string) {
	t.Helper()
	now := time.Now().UTC()
	err := tenancy.WithTenant(context.Background(), db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, db.Rebind(
			`INSERT INTO `+metadata.TableName("Account")+
				` (id, tenant_id, code, name, type, custom_fields, created_at, updated_at)
			  VALUES (?, ?, ?, ?, ?, '{}', ?, ?)`),
			idgen.New(), string(tenant), code, "Legacy", accountType, now, now)
		return err
	})
	if err != nil {
		t.Fatalf("seed legacy account: %v", err)
	}
}

func registeredNames(t *testing.T, db *storage.DB) []string {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), db.Rebind(
		`SELECT DISTINCT name FROM object_schemas WHERE layer = ? ORDER BY name`), "core")
	if err != nil {
		t.Fatalf("list object_schemas: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

// assertProblemJSON keeps the edge contract honest: a validation refusal is an
// RFC 7807 document naming the field, not an opaque 500.
func assertProblemJSON(t *testing.T, e *env, body []byte) {
	t.Helper()
	var p struct {
		Type   string `json:"type"`
		Title  string `json:"title"`
		Status int    `json:"status"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("response is not JSON: %s", body)
	}
	if p.Status != http.StatusUnprocessableEntity || p.Title == "" {
		t.Errorf("problem document = %+v, want a 422 with a title", p)
	}
	if p.Detail == "" {
		t.Error("problem document has no detail; a caller cannot tell which field was wrong")
	}
}
