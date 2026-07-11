package integrity

import (
	"context"
	"database/sql"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/iamdoubz/lasterp/kernel/eventstore"
	"github.com/iamdoubz/lasterp/kernel/idgen"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/storage/migrate"
	"github.com/iamdoubz/lasterp/kernel/storage/postgres"
	"github.com/iamdoubz/lasterp/kernel/storage/sqlite"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// harness is one migrated database per dialect, exposing both the restricted
// app role and the owner. The adversarial suite needs both: the app role
// proves role separation (UPDATE/DELETE denied by lack of grant), the owner
// proves the trigger still rejects a mutation from a connection that *does*
// hold the grant (defense in depth, and the only guard on SQLite).
type harness struct {
	dialect string
	app     *storage.DB // role-separated on Postgres; the sole handle on SQLite
	owner   *storage.DB // superuser on Postgres; == app on SQLite
}

func harnesses(t *testing.T) []harness {
	t.Helper()
	out := []harness{sqliteHarness(t)}
	if !testing.Short() {
		out = append(out, postgresHarness(t))
	}
	return out
}

func sqliteHarness(t *testing.T) harness {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "integrity.db") + "?_pragma=busy_timeout(30000)"
	db, err := sqlite.Open(dsn)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := migrate.Apply(context.Background(), db); err != nil {
		t.Fatalf("migrate sqlite: %v", err)
	}
	// No roles on SQLite: EnforceAppendOnlyGrants is a no-op and the owner
	// and app connections are one and the same (ADR-005 solo mode).
	if err := EnforceAppendOnlyGrants(context.Background(), db, "unused"); err != nil {
		t.Fatalf("EnforceAppendOnlyGrants (sqlite no-op): %v", err)
	}
	return harness{dialect: "sqlite", app: db, owner: db}
}

func postgresHarness(t *testing.T) harness {
	t.Helper()
	ctx := context.Background()

	container, err := tcpostgres.Run(ctx, "postgres:18",
		tcpostgres.WithDatabase("lasterp_integrity"),
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

	ownerDB, err := postgres.Open(dsn)
	if err != nil {
		t.Fatalf("open postgres (owner): %v", err)
	}
	t.Cleanup(func() { _ = ownerDB.Close() })
	if err := migrate.Apply(ctx, ownerDB); err != nil {
		t.Fatalf("migrate postgres: %v", err)
	}

	// Baseline app role: broad DML + sequence usage, exactly like the other
	// kernel packages' harnesses — then WP-0.8's role separation on top.
	const appUser, appPassword = "lasterp_app", "lasterp_app"
	for _, stmt := range []string{
		`CREATE ROLE ` + appUser + ` LOGIN PASSWORD '` + appPassword + `' NOSUPERUSER NOBYPASSRLS`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO ` + appUser,
		`GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO ` + appUser,
	} {
		if _, err := ownerDB.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("provision app role (%q): %v", stmt, err)
		}
	}
	if err := EnforceAppendOnlyGrants(ctx, ownerDB, appUser); err != nil {
		t.Fatalf("EnforceAppendOnlyGrants: %v", err)
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

	return harness{dialect: "postgres", app: appDB, owner: ownerDB}
}

func mustCreateTenant(t *testing.T, db *storage.DB) tenancy.ID {
	t.Helper()
	id := tenancy.ID(idgen.New())
	if err := tenancy.CreateTenant(context.Background(), db, id, "test tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	return id
}

// seedEvent appends one event through the real pipeline (as the app role),
// returning its id — a target row for the append-only mutation attempts.
func seedEvent(t *testing.T, db *storage.DB, tenant tenancy.ID) int64 {
	t.Helper()
	ev, err := eventstore.Append(context.Background(), db, tenant, eventstore.StreamID("s:"+idgen.New()), 0, idgen.New(),
		eventstore.NewEvent{Type: "seed.created", SchemaVersion: 1, Payload: []byte(`{}`), ActorID: "seed-actor"})
	if err != nil {
		t.Fatalf("seed event: %v", err)
	}
	return ev.ID
}

// seedAuditRow inserts one audit_log row directly (INSERT is still granted),
// returning its id — a target for the audit_log mutation attempts.
func seedAuditRow(t *testing.T, db *storage.DB, tenant tenancy.ID) string {
	t.Helper()
	id := idgen.New()
	err := tenancy.WithTenant(context.Background(), db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, db.Rebind(
			`INSERT INTO audit_log (id, tenant_id, object, record_id, action, changes, actor_id, at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
			id, string(tenant), "seed", idgen.New(), "create", "{}", "seed-actor", time.Now().UTC())
		return err
	})
	if err != nil {
		t.Fatalf("seed audit row: %v", err)
	}
	return id
}
