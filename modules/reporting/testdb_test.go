//go:build integrity

// SPDX-License-Identifier: AGPL-3.0-only

package reporting

import (
	"context"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/iamdoubz/lasterp/kernel/authz"
	"github.com/iamdoubz/lasterp/kernel/identity"
	"github.com/iamdoubz/lasterp/kernel/idgen"
	"github.com/iamdoubz/lasterp/kernel/integrity"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/storage/migrate"
	"github.com/iamdoubz/lasterp/kernel/storage/postgres"
	"github.com/iamdoubz/lasterp/kernel/storage/sqlite"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
	"github.com/iamdoubz/lasterp/modules/contacts"
	"github.com/iamdoubz/lasterp/modules/invoicing"
	"github.com/iamdoubz/lasterp/modules/ledger"
	"github.com/iamdoubz/lasterp/modules/tax"
)

func testDialects(t *testing.T) map[string]*storage.DB {
	t.Helper()
	dbs := map[string]*storage.DB{"sqlite": testSQLiteDB(t)}
	if !testing.Short() {
		dbs["postgres"] = testPostgresDB(t)
	}
	return dbs
}

func testSQLiteDB(t *testing.T) *storage.DB {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "reporting.db") + "?_pragma=busy_timeout(30000)"
	db, err := sqlite.Open(dsn)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := migrate.Apply(context.Background(), db); err != nil {
		t.Fatalf("migrate sqlite: %v", err)
	}
	return db
}

// testPostgresDB runs the reporting tests under the real deployment posture: an
// ordinary NOSUPERUSER NOBYPASSRLS app role with the event log locked down
// (append-only + ledger-pipeline grants), so posting an invoice provably works
// only through the pipeline (INV-F5). Mirrors modules/ledger's harness.
func testPostgresDB(t *testing.T) *storage.DB {
	t.Helper()
	ctx := context.Background()

	container, err := tcpostgres.Run(ctx, "postgres:18",
		tcpostgres.WithDatabase("lasterp_reporting"),
		tcpostgres.WithUsername("lasterp"),
		tcpostgres.WithPassword("lasterp"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").WithOccurrence(2).WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(container); err != nil {
			t.Logf("terminate container: %v", err)
		}
	})

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	superDB, err := postgres.Open(dsn)
	if err != nil {
		t.Fatalf("open postgres (superuser): %v", err)
	}
	defer func() { _ = superDB.Close() }()
	if err := migrate.Apply(ctx, superDB); err != nil {
		t.Fatalf("migrate postgres: %v", err)
	}

	const appUser, appPassword = "lasterp_app", "lasterp_app"
	if _, err := superDB.ExecContext(ctx, `CREATE ROLE `+appUser+` LOGIN PASSWORD '`+appPassword+`' NOSUPERUSER NOBYPASSRLS`); err != nil {
		t.Fatalf("create app role: %v", err)
	}
	if _, err := superDB.ExecContext(ctx, `GRANT USAGE, CREATE ON SCHEMA public TO `+appUser); err != nil {
		t.Fatalf("grant schema create to app role: %v", err)
	}
	if _, err := superDB.ExecContext(ctx, `GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO `+appUser); err != nil {
		t.Fatalf("grant to app role: %v", err)
	}
	// events.id is a BIGSERIAL — the app role needs its sequence to INSERT.
	if _, err := superDB.ExecContext(ctx, `GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO `+appUser); err != nil {
		t.Fatalf("grant sequences to app role: %v", err)
	}
	if err := integrity.EnforceAppendOnlyGrants(ctx, superDB, appUser); err != nil {
		t.Fatalf("EnforceAppendOnlyGrants: %v", err)
	}
	if err := integrity.EnforceLedgerPipelineGrants(ctx, superDB, appUser); err != nil {
		t.Fatalf("EnforceLedgerPipelineGrants: %v", err)
	}

	appDSN, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	appDSN.User = url.UserPassword(appUser, appPassword)

	appDB, err := postgres.Open(appDSN.String())
	if err != nil {
		t.Fatalf("open postgres (app role): %v", err)
	}
	t.Cleanup(func() { _ = appDB.Close() })
	return appDB
}

func mustCreateTenant(t *testing.T, db *storage.DB) tenancy.ID {
	t.Helper()
	id := tenancy.ID(idgen.New())
	if err := tenancy.CreateTenant(context.Background(), db, id, "test tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	return id
}

// invoicingActor creates a user with a role granting the given (object, actions)
// pairs and returns a context bound with that actor.
func reportingActor(t *testing.T, db *storage.DB, tenant tenancy.ID, grants map[string][]string) context.Context {
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
	role, err := authz.CreateRole(ctx, db, tenant, "rep-role-"+idgen.New(), false)
	if err != nil {
		t.Fatalf("CreateRole: %v", err)
	}
	for object, actions := range grants {
		for _, action := range actions {
			if err := authz.GrantPermission(ctx, db, tenant, role, object, action, ""); err != nil {
				t.Fatalf("GrantPermission(%s,%s): %v", object, action, err)
			}
		}
	}
	if err := authz.AssignRole(ctx, db, tenant, user.ID, role); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}
	return authz.WithActor(ctx, authz.Actor{TenantID: tenant, UserID: user.ID})
}

// --- reporting fixture ---

// writerGrants is everything needed to *create* the books the reports read.
func writerGrants() map[string][]string {
	return map[string][]string{
		ledger.ObjectAccount:      {"create", "read", "update", "delete"},
		ledger.ObjectPeriod:       {"create", "read", "update", "delete"},
		ledger.ObjectJournalEntry: {"post", "reverse", "read"},
		contacts.ObjectContact:    {"create", "read", "update", "delete"},
		invoicing.ObjectInvoice:   {"create", "read", "update", "post"},
		invoicing.ObjectReceipt:   {"create", "read", "update", "post"},
	}
}

// books is a seeded set of books: a chart of accounts, an open period, a
// customer, a tax rate, and the actor that built them.
type books struct {
	ctx        context.Context
	tenant     tenancy.ID
	period     string
	contactID  string
	arAccount  string
	revAccount string
	taxAccount string
	bankAcct   string
	capAccount string
}

// seedBooks registers the modules and creates the chart of accounts.
func seedBooks(t *testing.T, db *storage.DB) books {
	t.Helper()
	tenant := mustCreateTenant(t, db)
	for _, reg := range []struct {
		name string
		fn   func(context.Context, *storage.DB) error
	}{
		{"ledger", ledger.Register},
		{"contacts", contacts.Register},
		{"invoicing", invoicing.Register},
	} {
		if err := reg.fn(context.Background(), db); err != nil {
			t.Fatalf("Register %s: %v", reg.name, err)
		}
	}
	ctx := reportingActor(t, db, tenant, writerGrants())

	mk := func(code, name, typ string) string {
		acc, err := ledger.CreateAccount(ctx, db, tenant, code, name, typ, "", "")
		if err != nil {
			t.Fatalf("CreateAccount %s: %v", code, err)
		}
		return acc["id"].(string)
	}
	b := books{
		ctx: ctx, tenant: tenant, period: "2026-01",
		bankAcct:   mk("1000", "Bank", ledger.AccountAsset),
		arAccount:  mk("1100", "Accounts Receivable", ledger.AccountAsset),
		taxAccount: mk("2200", "Tax Payable", ledger.AccountLiability),
		capAccount: mk("3000", "Share Capital", ledger.AccountEquity),
		revAccount: mk("4000", "Revenue", ledger.AccountIncome),
	}
	if _, err := ledger.CreatePeriod(ctx, db, tenant, "2026-01", "2026-01-01", "2026-01-31"); err != nil {
		t.Fatalf("CreatePeriod: %v", err)
	}
	contact, err := contacts.CreateContact(ctx, db, tenant, "Acme Corp", idgen.New()+"@acme.example", contacts.KindCustomer)
	if err != nil {
		t.Fatalf("CreateContact: %v", err)
	}
	b.contactID = contact["id"].(string)

	if err := tax.SaveRate(ctx, db, tenant, tax.Rate{
		Jurisdiction: "DE", Category: tax.CategoryStandard, Rate: "0.20",
		// Effective well before any invoice date the aging tests use: the rate
		// store resolves by issue date, so a rate starting in 2026 would leave
		// back-dated invoices unpriceable.
		Rounding: tax.RoundHalfEven, AsOf: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
		Name: "VAT standard", Provider: "test",
	}); err != nil {
		t.Fatalf("SaveRate: %v", err)
	}
	return b
}

// postInvoice drafts and posts an invoice for qty × unitMinor, returning it.
func (b books) postInvoice(t *testing.T, db *storage.DB, issueDate string, qty, unitMinor int64) invoicing.Invoice {
	t.Helper()
	draft, err := invoicing.CreateDraft(b.ctx, db, b.tenant, invoicing.DraftInput{
		ContactID: b.contactID, Currency: "EUR", IssueDate: issueDate,
		ARAccount: b.arAccount, TaxAccount: b.taxAccount,
		Lines: []invoicing.Line{{
			Description: "Consulting", Quantity: qty, UnitPriceMinor: unitMinor,
			RevenueAccount: b.revAccount, TaxJurisdiction: "DE", TaxCategory: tax.CategoryStandard,
		}},
	})
	if err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	inv, err := invoicing.PostInvoice(b.ctx, db, b.tenant, draft.ID, b.period)
	if err != nil {
		t.Fatalf("PostInvoice: %v", err)
	}
	return inv
}

// receive posts a receipt settling amount against an invoice.
func (b books) receive(t *testing.T, db *storage.DB, invoiceID string, amount int64) {
	t.Helper()
	r, err := invoicing.CreateReceiptDraft(b.ctx, db, b.tenant, invoicing.ReceiptInput{
		ContactID: b.contactID, Currency: "EUR", ReceivedDate: "2026-01-20",
		BankAccount:  b.bankAcct,
		Applications: []invoicing.Application{{InvoiceID: invoiceID, AmountMinor: amount}},
	})
	if err != nil {
		t.Fatalf("CreateReceiptDraft: %v", err)
	}
	if _, err := invoicing.PostReceipt(b.ctx, db, b.tenant, r.ID, b.period); err != nil {
		t.Fatalf("PostReceipt: %v", err)
	}
}

// draftOnly creates an invoice draft that is never posted.
func draftOnly(t *testing.T, db *storage.DB, b books) (invoicing.Invoice, error) {
	t.Helper()
	return invoicing.CreateDraft(b.ctx, db, b.tenant, invoicing.DraftInput{
		ContactID: b.contactID, Currency: "EUR", IssueDate: "2026-01-05",
		ARAccount: b.arAccount, TaxAccount: b.taxAccount,
		Lines: []invoicing.Line{{
			Description: "Never posted", Quantity: 1, UnitPriceMinor: 99999,
			RevenueAccount: b.revAccount, TaxJurisdiction: "DE", TaxCategory: tax.CategoryStandard,
		}},
	})
}
