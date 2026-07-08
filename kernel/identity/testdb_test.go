package identity

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/storage/migrate"
	"github.com/iamdoubz/lasterp/kernel/storage/postgres"
	"github.com/iamdoubz/lasterp/kernel/storage/sqlite"
)

// testDialects returns one migrated *storage.DB per dialect the identity/
// authz property tests must pass on (CLAUDE.md: "adapter conformance suite
// must pass on Postgres AND SQLite for storage-touching code").
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
	dsn := filepath.Join(t.TempDir(), "identity.db")
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
		tcpostgres.WithDatabase("lasterp_identity"),
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
	db, err := postgres.Open(dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := migrate.Apply(ctx, db); err != nil {
		t.Fatalf("migrate postgres: %v", err)
	}
	return db
}
