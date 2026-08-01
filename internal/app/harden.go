// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/iamdoubz/lasterp/kernel/integrity"
	"github.com/iamdoubz/lasterp/kernel/storage"
)

// Deployment-time database role separation — docs/19 §2 layers 2 and 3.
//
// The grant helpers in kernel/integrity have existed since WP-0.8/WP-1.2 and
// until WP-1.10 were called from six test files and nothing else: no CLI, no
// chart, no compose file, no document. A real deployment therefore connected as
// the role that ran the migrations, which owns every table, and the REVOKEs were
// never applied. INV-F5 — "every financially-relevant document posts to GL
// through its declared template; no direct ledger writes outside the posting
// pipeline" — was consequently true in CI and false in production
// (phase-1-review.md P1 item 6).
//
// The append-only triggers on events and audit_log fire regardless of grants, so
// what was missing is defense in depth rather than the only guard. But the point
// of docs/19 is that a violation must beat all four layers, and a layer that
// only exists in a test harness is not a layer.
//
// This file is the shipped path. `lasterp harden` calls it, and so does the
// integrity test that proves it works — deliberately the same function, because
// a test that reimplements the deployment step proves the test works.

// appPrivileges are the grants the application role needs to run: read and write
// the ordinary tables, create the obj_* tables the metadata engine generates,
// and use the sequences behind them. Harden then takes back the ones that would
// let it bypass the write pipeline.
//
// Granting broadly and revoking precisely, rather than enumerating an allowlist,
// is deliberate: a new table added by a future migration is covered by default
// and a new *protected* table is covered by the catalog in kernel/integrity. The
// reverse would fail open — a forgotten grant breaks the app loudly, a forgotten
// revoke leaves a hole silently.
var appPrivileges = []string{
	"GRANT USAGE, CREATE ON SCHEMA public TO %s",
	"GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO %s",
	"GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO %s",
}

// ProvisionAppRole creates the restricted application role if it does not exist
// and grants it the privileges above. ownerDB must be a connection with rights
// to administer roles and grants — the migration or superuser connection, never
// the role being provisioned.
//
// NOSUPERUSER and NOBYPASSRLS are not decoration: Postgres never applies RLS to
// a superuser and skips it for a table's owner unless the table is FORCEd, so an
// application connecting as either makes INV-T1's storage backstop decorative.
// This is the generalization WP-0.3-decisions §9 called for after a tenancy test
// passed for exactly that wrong reason.
//
// Passing an empty password creates the role without one, for deployments that
// authenticate by peer, certificate or IAM rather than a secret.
func ProvisionAppRole(ctx context.Context, ownerDB *storage.DB, role, password string) error {
	if ownerDB.Dialect != storage.Postgres {
		return nil
	}
	if !integrity.ValidRoleName(role) {
		return fmt.Errorf("app: %q is not a valid role name", role)
	}

	// CREATE ROLE takes no bind parameters, so the password is a literal. With
	// standard_conforming_strings on (the default since Postgres 9.1) a
	// backslash is an ordinary character and doubling the single quote is the
	// complete escape. A NUL cannot appear in a Postgres string at all, so it is
	// rejected rather than mangled.
	if strings.ContainsRune(password, 0) {
		return fmt.Errorf("app: role password contains a NUL byte")
	}
	create := fmt.Sprintf("CREATE ROLE %s LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE", role)
	if password != "" {
		create += fmt.Sprintf(" PASSWORD '%s'", strings.ReplaceAll(password, "'", "''"))
	}
	if _, err := ownerDB.ExecContext(ctx, create); err != nil {
		// Re-running harden against a deployment that already has the role is
		// the normal case, not an error: this whole path has to be idempotent
		// because it runs on every deploy.
		if !isDuplicateRole(err) {
			return fmt.Errorf("app: create role %s: %w", role, err)
		}
		if password != "" {
			alter := fmt.Sprintf("ALTER ROLE %s WITH PASSWORD '%s'", role, strings.ReplaceAll(password, "'", "''"))
			if _, err := ownerDB.ExecContext(ctx, alter); err != nil {
				return fmt.Errorf("app: set password for existing role %s: %w", role, err)
			}
		}
	}

	for _, stmt := range appPrivileges {
		if _, err := ownerDB.ExecContext(ctx, fmt.Sprintf(stmt, role)); err != nil {
			return fmt.Errorf("app: grant privileges to %s: %w", role, err)
		}
	}
	return nil
}

// Harden applies the docs/19 role-separation revokes to role: the append-only
// tables lose UPDATE/DELETE/TRUNCATE (INV-E1, INV-T4) and the events table loses
// direct INSERT in favour of EXECUTE on the pipeline functions (INV-F5).
//
// It must run after Migrate — the SECURITY DEFINER functions it grants EXECUTE
// on are created by migration 0029 — and it is idempotent, so a deployment can
// run it on every boot without special-casing the first one.
func Harden(ctx context.Context, ownerDB *storage.DB, role string) error {
	if ownerDB.Dialect != storage.Postgres {
		// SQLite has no roles, and solo mode is a single trusted process
		// (ADR-005). The append-only triggers are the whole enforcement there,
		// which Diagnose reports rather than silently implying separation.
		return nil
	}
	if !integrity.ValidRoleName(role) {
		return fmt.Errorf("app: %q is not a valid role name", role)
	}
	if err := integrity.EnforceAppendOnlyGrants(ctx, ownerDB, role); err != nil {
		return err
	}
	return integrity.EnforceLedgerPipelineGrants(ctx, ownerDB, role)
}

// isDuplicateRole reports whether err is Postgres' duplicate_object (42710),
// which CREATE ROLE returns when the role is already there.
func isDuplicateRole(err error) bool {
	return err != nil && strings.Contains(err.Error(), "42710")
}

// Posture is what `lasterp doctor` reports: whether the connection the
// application actually uses is separated from the pipeline it is supposed to go
// through.
type Posture struct {
	Dialect string `json:"dialect"`
	Role    string `json:"role"`
	// Superuser or BypassRLS being true means RLS is not enforced against this
	// connection at all, which makes INV-T1's storage backstop decorative.
	Superuser bool `json:"superuser"`
	BypassRLS bool `json:"bypass_rls"`
	// CanInsertEvents true means the app role can write the event log directly,
	// bypassing append_event and ledger_post_entry — the INV-F5 hole.
	CanInsertEvents bool `json:"can_insert_events"`
	// CanMutateEvents / CanMutateAuditLog true means the append-only tables are
	// guarded only by their triggers (INV-E1, INV-T4).
	CanMutateEvents   bool `json:"can_mutate_events"`
	CanMutateAuditLog bool `json:"can_mutate_audit_log"`
	// Separated is the summary: false means this deployment is running with the
	// storage layer's guarantees weaker than the gauntlet proves them to be.
	Separated bool `json:"separated"`
	// Findings explains, in the operator's terms, whatever is not separated.
	Findings []string `json:"findings,omitempty"`
}

// Diagnose reports the posture of the connection it is given — which should be
// the one the application serves with, not an owner connection.
//
// It reads the catalog (has_table_privilege, pg_roles) rather than attempting a
// forbidden write. A diagnostic that works by trying to violate an invariant is
// a diagnostic that will one day succeed (WP-1.10-decisions.md §8).
func Diagnose(ctx context.Context, db *storage.DB) (Posture, error) {
	p := Posture{Dialect: db.Dialect.String()}
	if db.Dialect != storage.Postgres {
		p.Separated = true
		p.Findings = append(p.Findings,
			"SQLite has no role system; solo mode is a single trusted process (ADR-005). "+
				"The append-only triggers on events and audit_log are the whole enforcement here.")
		return p, nil
	}

	row := db.QueryRowContext(ctx, `
		SELECT current_user,
		       COALESCE((SELECT rolsuper      FROM pg_roles WHERE rolname = current_user), false),
		       COALESCE((SELECT rolbypassrls  FROM pg_roles WHERE rolname = current_user), false),
		       has_table_privilege(current_user, 'events',    'INSERT'),
		       has_table_privilege(current_user, 'events',    'UPDATE'),
		       has_table_privilege(current_user, 'audit_log', 'UPDATE')`)
	if err := row.Scan(&p.Role, &p.Superuser, &p.BypassRLS,
		&p.CanInsertEvents, &p.CanMutateEvents, &p.CanMutateAuditLog); err != nil {
		return Posture{}, fmt.Errorf("app: diagnose role posture: %w", err)
	}

	if p.Superuser {
		p.Findings = append(p.Findings,
			"connected as a superuser: Postgres never applies RLS to a superuser, so tenant isolation (INV-T1) has no storage backstop")
	}
	if p.BypassRLS {
		p.Findings = append(p.Findings,
			"role has BYPASSRLS: tenant isolation (INV-T1) has no storage backstop")
	}
	if p.CanInsertEvents {
		p.Findings = append(p.Findings,
			"role can INSERT directly into events: the ledger pipeline can be bypassed (INV-F5). Run `lasterp harden`")
	}
	if p.CanMutateEvents {
		p.Findings = append(p.Findings,
			"role can UPDATE events: the append-only log is guarded only by its trigger (INV-E1). Run `lasterp harden`")
	}
	if p.CanMutateAuditLog {
		p.Findings = append(p.Findings,
			"role can UPDATE audit_log: the audit trail is guarded only by its trigger (INV-T4). Run `lasterp harden`")
	}
	p.Separated = len(p.Findings) == 0
	return p, nil
}
