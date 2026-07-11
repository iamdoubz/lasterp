// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"net/http"
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
	"github.com/iamdoubz/lasterp/kernel/metadata"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/storage/migrate"
	"github.com/iamdoubz/lasterp/kernel/storage/postgres"
	"github.com/iamdoubz/lasterp/kernel/storage/sqlite"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// contactYAML is the api package's copy of the sample object (the metadata
// package's fixture is unexported) — a CRUD object the gateway routes.
const contactYAML = `
object: Contact
module: crm
persistence: crud
fields:
  - {name: full_name, type: text, required: true}
  - {name: email, type: email}
  - {name: newsletter_opt_in, type: bool}
permissions:
  read: [crm.viewer]
  create: [crm.user]
  update: [crm.user]
  delete: [crm.admin]
`

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
	dsn := filepath.Join(t.TempDir(), "api.db") + "?_pragma=busy_timeout(30000)"
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

// testPostgresDB mirrors kernel/metadata's helper: migrate as the container
// superuser, then hand back a NOSUPERUSER NOBYPASSRLS role so RLS is real.
func testPostgresDB(t *testing.T) *storage.DB {
	t.Helper()
	ctx := context.Background()

	container, err := tcpostgres.Run(ctx, "postgres:18",
		tcpostgres.WithDatabase("lasterp_api"),
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

// contactSchema parses + merges the sample Contact object and applies its DDL.
func contactSchema(t *testing.T, db *storage.DB) *metadata.EffectiveSchema {
	t.Helper()
	core, err := metadata.ParseObject([]byte(contactYAML))
	if err != nil {
		t.Fatalf("ParseObject: %v", err)
	}
	eff, err := metadata.Merge(core)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if err := metadata.ApplyDDL(context.Background(), db, eff, 1); err != nil {
		t.Fatalf("ApplyDDL: %v", err)
	}
	return eff
}

// seedActor creates a user + role granting the given Contact actions and
// returns the actor the test authenticator will present.
func seedActor(t *testing.T, db *storage.DB, tenant tenancy.ID, actions ...string) authz.Actor {
	t.Helper()
	ctx := context.Background()
	hash, err := identity.HashPassword("s3cret!")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	user, err := identity.CreateUser(ctx, db, tenant, "actor@example.com", hash)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	role, err := authz.CreateRole(ctx, db, tenant, "contact-manager", false)
	if err != nil {
		t.Fatalf("CreateRole: %v", err)
	}
	for _, action := range actions {
		if err := authz.GrantPermission(ctx, db, tenant, role, "Contact", action, ""); err != nil {
			t.Fatalf("GrantPermission(%s): %v", action, err)
		}
	}
	if err := authz.AssignRole(ctx, db, tenant, user.ID, role); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}
	return authz.Actor{TenantID: tenant, UserID: user.ID}
}

// fixedAuth is a test Authenticator that presents a constant actor/tenant.
func fixedAuth(actor authz.Actor, tenant tenancy.ID) Authenticator {
	return AuthenticatorFunc(func(_ *http.Request) (authz.Actor, tenancy.ID, error) {
		return actor, tenant, nil
	})
}
