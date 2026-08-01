//go:build integrity

// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// WP-1.10 AC: "a Postgres e2e that boots serving as a restricted role and proves
// a direct INSERT INTO events is refused (42501) through the *shipped*
// deployment path, not a test fixture."
//
// The distinction is the whole point of this file. The grant helpers have been
// exercised since WP-0.8, but always by inline SQL written inside a test, which
// proved that a hand-written fixture could lock a role down — not that the
// product could. bootDBs now provisions and hardens through ProvisionAppRole and
// Harden, the same two functions `lasterp harden` calls, so every Postgres test
// in this package runs under the posture a real deployment gets.
//
// Invariants: INV-F5 (no direct ledger writes outside the posting pipeline),
// INV-E1 (streams append-only — "DB grants and triggers make it impossible"),
// INV-T4 (audit_log append-only).

// pgErrorCode returns the SQLSTATE of a Postgres error, or "".
func pgErrorCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// insufficientPrivilege is SQLSTATE 42501. Asserting the code rather than a
// substring of the message means the test cannot pass because the statement
// failed for some unrelated reason (a typo'd column, a missing table).
const insufficientPrivilege = "42501"

// TestHardenedRoleCannotWriteTheLogDirectly is the INV-F5 proof at the storage
// layer: after the shipped hardening path has run, the role the application
// serves as cannot append to the event log except through the pipeline
// functions, cannot mutate what is already there, and cannot rewrite the audit
// trail.
func TestHardenedRoleCannotWriteTheLogDirectly(t *testing.T) {
	db := postgresBootDB(t)

	tests := []struct {
		name string
		stmt string
		args []any
	}{
		{
			// The INV-F5 hole. Without REVOKE INSERT, application code could
			// write a ledger event that never passed the balance and
			// open-period checks in ledger_post_entry.
			name: "direct INSERT into events",
			stmt: `INSERT INTO events (tenant_id, stream_id, version, type, schema_version, payload, actor_id, command_id, occurred_at, recorded_at)
			       VALUES ('t', 's', 1, 'Posted', 1, '{}', 'a', 'c', NOW(), NOW())`,
		},
		{
			name: "UPDATE an existing event",
			stmt: `UPDATE events SET payload = '{}'`,
		},
		{
			name: "DELETE an event",
			stmt: `DELETE FROM events`,
		},
		{
			name: "UPDATE the audit trail",
			stmt: `UPDATE audit_log SET action = 'nothing to see here'`,
		},
		{
			name: "DELETE from the audit trail",
			stmt: `DELETE FROM audit_log`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := db.ExecContext(context.Background(), tc.stmt, tc.args...)
			if err == nil {
				t.Fatalf("%s succeeded as the application role — role separation is not in effect", tc.name)
			}
			if got := pgErrorCode(err); got != insufficientPrivilege {
				t.Errorf("SQLSTATE = %q, want %q (insufficient_privilege); err = %v", got, insufficientPrivilege, err)
			}
		})
	}
}

// TestHardenedRoleStillPostsThroughThePipeline is the other half: hardening must
// lock down the bypass without breaking the product. A REVOKE that also stopped
// legitimate posting would be caught here rather than in a deployment.
//
// It reuses the full lifecycle harness, which runs against the hardened role, so
// "the pipeline still works" is proven by an invoice actually reaching the
// general ledger over HTTP.
func TestHardenedRoleStillPostsThroughThePipeline(t *testing.T) {
	db := postgresBootDB(t)
	e := seed(t, db)

	arID := e.createAccount("1100", "Accounts Receivable", "asset")
	revID := e.createAccount("4000", "Sales Revenue", "income")
	taxID := e.createAccount("2200", "Tax Payable", "liability")

	st, _, contact := e.post("/api/v1/contact", map[string]any{
		"name": "Acme Co", "email": "ap@acme.example", "kind": "customer",
	})
	if st != 201 {
		t.Fatalf("create contact status = %d", st)
	}
	st, _, _ = e.post("/api/v1/periods", map[string]any{
		"code": "2026-07", "start_date": "2026-07-01", "end_date": "2026-07-31",
	})
	if st != 201 {
		t.Fatalf("create period status = %d", st)
	}

	st, body, _ := e.post("/api/v1/taxrates", map[string]any{
		"jurisdiction": "US-CA", "category": "sales", "rate": "0.10",
		"rounding": "half_even", "as_of": "2026-01-01", "name": "CA sales",
	})
	if st != 201 {
		t.Fatalf("create tax rate status = %d: %s", st, body)
	}

	var created map[string]any
	st, body, created = e.post("/api/v1/invoices", map[string]any{
		"contact_id": mustField(t, contact, "id"), "currency": "USD", "issue_date": "2026-07-15",
		"ar_account": arID, "tax_account": taxID,
		"lines": []map[string]any{{
			"description": "Consulting", "quantity": 1, "unit_price_minor": 10000,
			"revenue_account": revID, "tax_jurisdiction": "US-CA", "tax_category": "sales",
		}},
	})
	if st != 201 {
		t.Fatalf("create draft status = %d: %s", st, body)
	}

	st, body, posted := e.post("/api/v1/invoices/"+mustField(t, created, "ID")+"/post", map[string]any{"period": "2026-07"})
	if st != 200 {
		t.Fatalf("post invoice under a hardened role = %d: %s", st, body)
	}
	if posted["GLEntryID"] == nil || posted["GLEntryID"] == "" {
		t.Fatalf("posted invoice has no GL entry: %v", posted)
	}
}

// TestDiagnoseReportsPosture covers `lasterp doctor`: it must say "separated"
// for the hardened role and name the specific holes for an unhardened one.
// Otherwise the command is decoration — an operator running it would get a
// reassuring answer either way.
func TestDiagnoseReportsPosture(t *testing.T) {
	t.Run("hardened application role", func(t *testing.T) {
		p, err := Diagnose(context.Background(), postgresBootDB(t))
		if err != nil {
			t.Fatalf("Diagnose: %v", err)
		}
		if !p.Separated {
			t.Errorf("hardened role reported as not separated: %v", p.Findings)
		}
		for name, got := range map[string]bool{
			"superuser": p.Superuser, "bypass RLS": p.BypassRLS,
			"can insert events": p.CanInsertEvents, "can mutate events": p.CanMutateEvents,
			"can mutate audit_log": p.CanMutateAuditLog,
		} {
			if got {
				t.Errorf("hardened role %s = true, want false", name)
			}
		}
	})

	t.Run("owner role is not separated", func(t *testing.T) {
		// The posture a deployment has today if it never runs `lasterp harden`:
		// serving as the role that ran the migrations. doctor must refuse to
		// call that separated, which is the finding this whole WP exists for.
		owner := postgresOwnerDB(t)
		p, err := Diagnose(context.Background(), owner)
		if err != nil {
			t.Fatalf("Diagnose: %v", err)
		}
		if p.Separated {
			t.Fatal("an unhardened owner connection was reported as separated — doctor would tell an operator their deployment is fine when it is not")
		}
		if len(p.Findings) == 0 {
			t.Error("not separated but no findings: doctor must say what is wrong, not just that something is")
		}
	})

	t.Run("sqlite reports why separation does not apply", func(t *testing.T) {
		p, err := Diagnose(context.Background(), sqliteBootDB(t))
		if err != nil {
			t.Fatalf("Diagnose: %v", err)
		}
		// Solo mode is a single trusted process (ADR-005), so "separated" is
		// honest — but it must come with the reason rather than implying a role
		// system that is not there.
		if !p.Separated || len(p.Findings) == 0 {
			t.Errorf("sqlite posture = %+v, want separated with an explanation", p)
		}
	})
}

// TestHardenIsIdempotent: a deployment runs harden on every boot rather than
// tracking whether it is the first, so running it twice must be a no-op and not
// an error.
func TestHardenIsIdempotent(t *testing.T) {
	ctx := context.Background()
	owner := postgresOwnerDB(t)

	for i := range 2 {
		if err := ProvisionAppRole(ctx, owner, "lasterp_idem", "pw"); err != nil {
			t.Fatalf("ProvisionAppRole run %d: %v", i+1, err)
		}
		if err := Harden(ctx, owner, "lasterp_idem"); err != nil {
			t.Fatalf("Harden run %d: %v", i+1, err)
		}
	}
}

// TestHardenRejectsUnsafeRoleNames: role names cannot be bind parameters in
// GRANT/REVOKE/CREATE ROLE, so they are interpolated. Anything that is not a
// plain SQL identifier must be refused before it reaches the database.
func TestHardenRejectsUnsafeRoleNames(t *testing.T) {
	ctx := context.Background()
	owner := postgresOwnerDB(t)

	for _, name := range []string{
		`app"; DROP TABLE events; --`,
		`app role`,
		`1app`,
		``,
		`app-role`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := Harden(ctx, owner, name); err == nil {
				t.Errorf("Harden accepted %q as a role name", name)
			}
			if err := ProvisionAppRole(ctx, owner, name, "pw"); err == nil {
				t.Errorf("ProvisionAppRole accepted %q as a role name", name)
			}
		})
	}
}

// TestProvisionRejectsNulInPassword: a NUL cannot appear in a Postgres string
// literal, and the password is interpolated because CREATE ROLE takes no bind
// parameters. Reject rather than truncate.
func TestProvisionRejectsNulInPassword(t *testing.T) {
	if err := ProvisionAppRole(context.Background(), postgresOwnerDB(t), "lasterp_nul", "a\x00b"); err == nil {
		t.Error("ProvisionAppRole accepted a password containing a NUL byte")
	}
}

// TestProvisionEscapesQuotesInPassword: the quote-doubling escape must produce a
// role that can actually log in with the password it was given, and must not let
// the password terminate the literal.
func TestProvisionEscapesQuotesInPassword(t *testing.T) {
	ctx := context.Background()
	f := postgresBoot(t)
	owner := f.owner
	const role, pw = "lasterp_quoted", "it's a 'quoted' password"

	if err := ProvisionAppRole(ctx, owner, role, pw); err != nil {
		t.Fatalf("ProvisionAppRole with a quoted password: %v", err)
	}
	// The role exists and the statement did not fragment into extra commands.
	var n int
	if err := owner.QueryRowContext(ctx,
		`SELECT count(*) FROM pg_roles WHERE rolname = $1`, role).Scan(&n); err != nil {
		t.Fatalf("count role: %v", err)
	}
	if n != 1 {
		t.Fatalf("role count = %d, want 1", n)
	}
	if err := f.connectAs(t, role, pw); err != nil {
		t.Errorf("cannot log in with the escaped password: %v", err)
	}
}
