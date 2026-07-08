package tenancy

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/iamdoubz/lasterp/kernel/idgen"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/storage/migrate"
	"github.com/iamdoubz/lasterp/kernel/storage/postgres"
)

// RLS (and therefore INV-T1) is a Postgres-only mechanism (ADR-005: SQLite
// solo mode bypasses RLS). This suite is Postgres-specific by design.
//
// Row security is never applied to superusers, and the testcontainers
// Postgres module's default user is a superuser (it initializes the
// cluster) — so migrating and querying as that same role would make every
// RLS assertion here a false positive. testPostgresDB migrates as the
// superuser, then hands back a connection as a freshly created ordinary
// (NOSUPERUSER NOBYPASSRLS) role, the way an application is meant to
// connect. Full production role separation is WP-0.8's job; this is the
// minimum needed for the RLS tests to mean anything.
func testPostgresDB(t *testing.T) *storage.DB {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping testcontainers-backed test in -short mode")
	}
	ctx := context.Background()

	container, err := tcpostgres.Run(ctx, "postgres:18",
		tcpostgres.WithDatabase("lasterp_tenancy"),
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
	if _, err := superDB.ExecContext(ctx, `GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO `+appUser); err != nil {
		t.Fatalf("grant to app role: %v", err)
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

func seedUser(t *testing.T, db *storage.DB, tenant ID) {
	t.Helper()
	ctx := context.Background()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	ctx, err = SetContext(ctx, tx, db.Dialect, tenant)
	if err != nil {
		t.Fatalf("SetContext: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO tenants (id, name, created_at) VALUES ($1, $2, $3)`,
		string(tenant), "seed", time.Now().UTC()); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO users (id, tenant_id, email, created_at) VALUES ($1, $2, $3, $4)`,
		idgen.New(), string(tenant), "seed@example.com", time.Now().UTC()); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func countUsers(t *testing.T, db *storage.DB, ctx context.Context) int {
	t.Helper()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		t.Fatalf("count users: %v", err)
	}
	return n
}

// INV-T1: no query path returns another tenant's rows; a query with no
// tenant context set returns zero rows even though matching data exists.
func TestNoContextZeroRows(t *testing.T) {
	db := testPostgresDB(t)
	seedUser(t, db, ID(idgen.New()))

	if n := countUsers(t, db, context.Background()); n != 0 {
		t.Fatalf("users visible with no tenant context set: count = %d, want 0", n)
	}
}

// INV-T1: tenant A's context never surfaces tenant B's rows.
func TestCrossTenantIsolation(t *testing.T) {
	db := testPostgresDB(t)
	tenantA := ID(idgen.New())
	tenantB := ID(idgen.New())
	seedUser(t, db, tenantA)
	seedUser(t, db, tenantB)

	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	ctxA, err := SetContext(ctx, tx, db.Dialect, tenantA)
	if err != nil {
		t.Fatalf("SetContext: %v", err)
	}
	var n int
	if err := tx.QueryRowContext(ctxA, `SELECT COUNT(*) FROM users WHERE tenant_id = $1`, string(tenantB)).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("tenant A context saw %d of tenant B's users, want 0", n)
	}

	if err := tx.QueryRowContext(ctxA, `SELECT COUNT(*) FROM users WHERE tenant_id = $1`, string(tenantA)).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("tenant A context saw %d of its own users, want 1", n)
	}
}
