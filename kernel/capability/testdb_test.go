package capability

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
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/storage/migrate"
	"github.com/iamdoubz/lasterp/kernel/storage/postgres"
	"github.com/iamdoubz/lasterp/kernel/storage/sqlite"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// testDialects returns one migrated *storage.DB per dialect, Postgres as a
// non-superuser app role so RLS on module_state is actually enforced (see
// kernel/metadata's identical helper and docs/notes/WP-0.4-decisions.md).
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
	dsn := filepath.Join(t.TempDir(), "capability.db") + "?_pragma=busy_timeout(30000)"
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

func testPostgresDB(t *testing.T) *storage.DB {
	t.Helper()
	ctx := context.Background()

	container, err := tcpostgres.Run(ctx, "postgres:18",
		tcpostgres.WithDatabase("lasterp_capability"),
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
	for _, stmt := range []string{
		`CREATE ROLE ` + appUser + ` LOGIN PASSWORD '` + appPassword + `' NOSUPERUSER NOBYPASSRLS`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO ` + appUser,
		`GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO ` + appUser,
	} {
		if _, err := superDB.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("provision app role (%q): %v", stmt, err)
		}
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

// authorizedCtx returns a context carrying an actor granted the
// (capability, manage) permission — what Enable/Disable/ApplyProfile require.
func authorizedCtx(t *testing.T, db *storage.DB, tenant tenancy.ID) context.Context {
	t.Helper()
	ctx := context.Background()
	role, err := authz.CreateRole(ctx, db, tenant, "cap-admin", false)
	if err != nil {
		t.Fatalf("create role: %v", err)
	}
	if err := authz.GrantPermission(ctx, db, tenant, role, authzObject, authzAction, ""); err != nil {
		t.Fatalf("grant: %v", err)
	}
	user := identity.UserID(idgen.New())
	if err := authz.AssignRole(ctx, db, tenant, user, role); err != nil {
		t.Fatalf("assign role: %v", err)
	}
	return authz.WithActor(ctx, authz.Actor{TenantID: tenant, UserID: user})
}
