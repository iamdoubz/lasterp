package invoicing

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
	dsn := filepath.Join(t.TempDir(), "invoicing.db") + "?_pragma=busy_timeout(30000)"
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

// testPostgresDB runs the invoicing tests under the real deployment posture: an
// ordinary NOSUPERUSER NOBYPASSRLS app role with the event log locked down
// (append-only + ledger-pipeline grants), so posting an invoice provably works
// only through the pipeline (INV-F5). Mirrors modules/ledger's harness.
func testPostgresDB(t *testing.T) *storage.DB {
	t.Helper()
	ctx := context.Background()

	container, err := tcpostgres.Run(ctx, "postgres:18",
		tcpostgres.WithDatabase("lasterp_invoicing"),
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
func invoicingActor(t *testing.T, db *storage.DB, tenant tenancy.ID, grants map[string][]string) context.Context {
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
	role, err := authz.CreateRole(ctx, db, tenant, "inv-role-"+idgen.New(), false)
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

// fullGrants is every (object, action) the DB-backed invoicing flow needs: it
// creates the chart of accounts and period, the contact, the invoice, posts the
// GL entry (JournalEntry "post"), and reads results back.
func fullGrants() map[string][]string {
	return map[string][]string{
		ledger.ObjectAccount:      {"create", "read", "update", "delete"},
		ledger.ObjectPeriod:       {"create", "read", "update", "delete"},
		ledger.ObjectJournalEntry: {"post", "reverse", "read"},
		contacts.ObjectContact:    {"create", "read", "update", "delete"},
		ObjectInvoice:             {"create", "read", "update", "post"},
	}
}

// fixture is a fully seeded invoicing environment: registered schemas, a chart
// of accounts (AR / revenue / tax payable), an open period, a customer contact,
// and a tax rate on file.
type fixture struct {
	ctx        context.Context
	tenant     tenancy.ID
	period     string
	contactID  string
	arAccount  string
	revAccount string
	taxAccount string
}

// setup registers ledger + contacts + invoicing schemas and seeds the fixture.
func setup(t *testing.T, db *storage.DB) fixture {
	t.Helper()
	tenant := mustCreateTenant(t, db)
	for _, reg := range []struct {
		name string
		fn   func(context.Context, *storage.DB) error
	}{
		{"ledger", ledger.Register},
		{"contacts", contacts.Register},
		{"invoicing", Register},
	} {
		if err := reg.fn(context.Background(), db); err != nil {
			t.Fatalf("Register %s: %v", reg.name, err)
		}
	}
	ctx := invoicingActor(t, db, tenant, fullGrants())

	ar, err := ledger.CreateAccount(ctx, db, tenant, "1100", "Accounts Receivable", ledger.AccountAsset, "", "")
	if err != nil {
		t.Fatalf("CreateAccount AR: %v", err)
	}
	rev, err := ledger.CreateAccount(ctx, db, tenant, "4000", "Revenue", ledger.AccountIncome, "", "")
	if err != nil {
		t.Fatalf("CreateAccount revenue: %v", err)
	}
	taxPayable, err := ledger.CreateAccount(ctx, db, tenant, "2200", "Tax Payable", ledger.AccountLiability, "", "")
	if err != nil {
		t.Fatalf("CreateAccount tax payable: %v", err)
	}
	if _, err := ledger.CreatePeriod(ctx, db, tenant, "2026-01", "2026-01-01", "2026-01-31"); err != nil {
		t.Fatalf("CreatePeriod: %v", err)
	}
	contact, err := contacts.CreateContact(ctx, db, tenant, "Acme Corp", "ap@acme.example", contacts.KindCustomer)
	if err != nil {
		t.Fatalf("CreateContact: %v", err)
	}
	// A 20% standard rate on file for the invoice's jurisdiction/category.
	if err := tax.SaveRate(ctx, db, tenant, tax.Rate{
		Jurisdiction: "DE", Category: tax.CategoryStandard, Rate: "0.20",
		Rounding: tax.RoundHalfEven, AsOf: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Name: "VAT standard", Provider: "test",
	}); err != nil {
		t.Fatalf("SaveRate: %v", err)
	}

	return fixture{
		ctx: ctx, tenant: tenant, period: "2026-01",
		contactID:  contact["id"].(string),
		arAccount:  ar["id"].(string),
		revAccount: rev["id"].(string),
		taxAccount: taxPayable["id"].(string),
	}
}

// draft returns a one-line DraftInput: qty units at unitMinor each, 20% DE VAT.
func (f fixture) draft(qty, unitMinor int64) DraftInput {
	return DraftInput{
		ContactID: f.contactID, Currency: "EUR", IssueDate: "2026-01-15",
		ARAccount: f.arAccount, TaxAccount: f.taxAccount,
		Lines: []Line{{
			Description: "Consulting", Quantity: qty, UnitPriceMinor: unitMinor,
			RevenueAccount: f.revAccount, TaxJurisdiction: "DE", TaxCategory: tax.CategoryStandard,
		}},
	}
}
