// SPDX-License-Identifier: AGPL-3.0-only

package plugins

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/iamdoubz/lasterp/kernel/authz"
	"github.com/iamdoubz/lasterp/kernel/identity"
	"github.com/iamdoubz/lasterp/kernel/idgen"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// WP-3.1a: the plugin host. Invariants: **INV-X1** (plugins touch data only
// via capability-checked host functions — no ambient authority), INV-T3 (an
// approval may narrow, never widen), INV-T4 (a plugin's actions are
// attributable to the plugin).
//
// The corpus these tests run is compiled from readable Go in testdata/ — see
// corpus_test.go for why it is not a directory of committed .wasm files.

const helloManifest = `
id: com.acme.hello
version: 1.0.0
functions: [echo, say, read, write, secret, chatter]
capabilities:
  objects:
    - {type: Widget, access: read}
`

// helloWith is the hello module's manifest with a chosen function list. It
// always grants every capability the module *imports*, because a WASM module
// whose imports are not all satisfied cannot be instantiated at all — see
// TestUngrantedHostFunctionsCannotEvenBeImported, which is that property used
// as the enforcement mechanism.
func helloWith(functions string) string {
	return `
id: com.acme.hello
version: 1.0.0
functions: [` + functions + `]
capabilities:
  objects:
    - {type: Widget, access: read}
    - {type: Widget, access: write}
  secrets: [acme_api_key]
`
}

// helloApprover holds every grant helloWith requests.
func helloApprover(t *testing.T, db *storage.DB, tenant tenancy.ID) authz.Actor {
	t.Helper()
	return approver(t, db, tenant,
		[2]string{"Widget", "read"}, [2]string{"Widget", "create"},
		[2]string{"Widget", "update"}, [2]string{"secret", "manage"})
}

// --- helpers ---

func newTenant(t *testing.T, db *storage.DB) tenancy.ID {
	t.Helper()
	id := tenancy.ID(idgen.New())
	if err := tenancy.CreateTenant(context.Background(), db, id, "plugin test"); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	return id
}

// approver returns an actor holding every permission in grants.
func approver(t *testing.T, db *storage.DB, tenant tenancy.ID, grants ...[2]string) authz.Actor {
	t.Helper()
	ctx := context.Background()
	user := identity.UserID(idgen.New())
	role, err := authz.CreateRole(ctx, db, tenant, "approver-"+string(user), false)
	if err != nil {
		t.Fatalf("CreateRole: %v", err)
	}
	for _, g := range grants {
		if err := authz.GrantPermission(ctx, db, tenant, role, g[0], g[1], ""); err != nil {
			t.Fatalf("GrantPermission: %v", err)
		}
	}
	if err := authz.AssignRole(ctx, db, tenant, user, role); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}
	return authz.Actor{TenantID: tenant, UserID: user}
}

func install(t *testing.T, db *storage.DB, tenant tenancy.ID, manifest string, module []byte, act authz.Actor) *Installed {
	t.Helper()
	p, err := Install(context.Background(), db, tenant, []byte(manifest), module, act)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	return p
}

// --- manifest ---

func TestManifestRefusesWhatThisHostCannotHonour(t *testing.T) {
	cases := map[string]string{
		"no id":        "version: 1.0.0\nfunctions: [run]\n",
		"bad id":       "id: Com.Acme!\nversion: 1.0.0\nfunctions: [run]\n",
		"no version":   "id: com.acme.x\nfunctions: [run]\n",
		"no functions": "id: com.acme.x\nversion: 1.0.0\n",
		"bad access":   "id: com.acme.x\nversion: 1.0.0\nfunctions: [run]\ncapabilities:\n  objects: [{type: Widget, access: admin}]\n",
		"unknown key":  "id: com.acme.x\nversion: 1.0.0\nfunctions: [run]\nsuperpowers: true\n",
		// Declared-but-unimplemented surfaces are refused, never ignored: a
		// plugin that "installs fine" and silently never fires is the failure
		// mode, and the same silence could one day drop a capability an
		// administrator believed they were reviewing.
		"hooks":    "id: com.acme.x\nversion: 1.0.0\nfunctions: [run]\nhooks: [{event: invoice.posted, fn: run}]\n",
		"http":     "id: com.acme.x\nversion: 1.0.0\nfunctions: [run]\ncapabilities:\n  http: [{host: api.acme.com}]\n",
		"schedule": "id: com.acme.x\nversion: 1.0.0\nfunctions: [run]\ncapabilities:\n  schedule: [\"0 2 * * *\"]\n",
		"mcp":      "id: com.acme.x\nversion: 1.0.0\nfunctions: [run]\nmcp_tools: [{name: x, fn: run}]\n",
	}
	for name, yaml := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseManifest([]byte(yaml)); err == nil {
				t.Error("accepted a manifest this host cannot honour exactly")
			}
		})
	}

	m, err := ParseManifest([]byte(helloManifest))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if m.Principal() != "plugin:com.acme.hello" {
		t.Errorf("principal = %q", m.Principal())
	}
	if got := m.Permissions(); len(got) != 1 || got[0] != [2]string{"Widget", "read"} {
		t.Errorf("permissions = %v", got)
	}
}

func TestWriteAccessGrantsCreateAndUpdateButNotRead(t *testing.T) {
	m, err := ParseManifest([]byte(`
id: com.acme.writer
version: 1.0.0
functions: [run]
capabilities:
  objects: [{type: Widget, access: write}]
`))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	// Least privilege, literally: a writer that also needs to read declares
	// both, because inferring the extra grant would widen what the
	// administrator approved.
	want := [][2]string{{"Widget", "create"}, {"Widget", "update"}}
	got := m.Permissions()
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("permissions = %v, want %v", got, want)
	}
}

// --- install: INV-T3, the only moment authority is created ---

func TestInstallRefusesCapabilitiesTheApproverDoesNotHold(t *testing.T) {
	for name, db := range testDialects(t) {
		t.Run(name, func(t *testing.T) {
			tenant := newTenant(t, db)
			// An administrator of something else entirely.
			weak := approver(t, db, tenant, [2]string{"Contact", "read"})

			_, err := Install(context.Background(), db, tenant, []byte(helloManifest),
				corpusModule(t, "hello"), weak)
			if !errors.Is(err, ErrCapabilityNotHeld) {
				t.Fatalf("Install = %v, want ErrCapabilityNotHeld", err)
			}
			if !strings.Contains(err.Error(), "Widget:read") {
				t.Errorf("error does not name the missing capability: %v", err)
			}

			// And nothing was left behind: no half-installed plugin, no role.
			if _, err := Get(context.Background(), db, tenant, "com.acme.hello"); !errors.Is(err, ErrNotFound) {
				t.Errorf("a refused install left a plugin row: %v", err)
			}
		})
	}
}

func TestInstallGrantsExactlyWhatTheManifestRequests(t *testing.T) {
	for name, db := range testDialects(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			tenant := newTenant(t, db)
			admin := approver(t, db, tenant,
				[2]string{"Widget", "read"}, [2]string{"Widget", "create"},
				[2]string{"Widget", "update"}, [2]string{"secret", "manage"})

			p := install(t, db, tenant, helloManifest, corpusModule(t, "hello"), admin)

			// The plugin holds its declared read...
			pluginActor := p.Actor(tenant)
			for _, tc := range []struct {
				object, action string
				want           bool
			}{
				{"Widget", "read", true},
				{"Widget", "create", false}, // declared read only
				{"Widget", "update", false},
				{"Contact", "read", false},
			} {
				got, err := authz.Can(ctx, db, pluginActor, tc.object, tc.action)
				if err != nil {
					t.Fatalf("Can: %v", err)
				}
				if got != tc.want {
					t.Errorf("INV-T3: plugin can %s:%s = %v, want %v", tc.object, tc.action, got, tc.want)
				}
			}

			// ...and its principal is not a user anyone can log in as.
			if _, err := identity.GetUserByEmail(ctx, db, tenant, p.Principal()); err == nil {
				t.Error("the plugin principal exists as a user row; nothing should be able to log in as a plugin")
			}
		})
	}
}

func TestUninstallTakesTheAuthorityWithIt(t *testing.T) {
	for name, db := range testDialects(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			tenant := newTenant(t, db)
			admin := approver(t, db, tenant, [2]string{"Widget", "read"})
			p := install(t, db, tenant, helloManifest, corpusModule(t, "hello"), admin)

			if err := Uninstall(ctx, db, tenant, p.ID, "admin-under-test"); err != nil {
				t.Fatalf("Uninstall: %v", err)
			}
			// A role left behind is authority a later install under the same id
			// would silently inherit.
			can, err := authz.Can(ctx, db, p.Actor(tenant), "Widget", "read")
			if err != nil {
				t.Fatalf("Can: %v", err)
			}
			if can {
				t.Error("INV-T3: an uninstalled plugin's principal still holds its grants")
			}
			if _, err := Get(ctx, db, tenant, p.ID); !errors.Is(err, ErrNotFound) {
				t.Errorf("Get after uninstall = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestInstallRefusesNonWASMAndOversizeModules(t *testing.T) {
	db := testSQLiteDB(t)
	tenant := newTenant(t, db)
	admin := approver(t, db, tenant, [2]string{"Widget", "read"})

	if _, err := Install(context.Background(), db, tenant, []byte(helloManifest),
		[]byte("#!/bin/sh\nrm -rf /\n"), admin); err == nil {
		t.Error("installed something that is not a WebAssembly binary")
	}
	if _, err := Install(context.Background(), db, tenant, []byte(helloManifest),
		make([]byte, MaxModuleBytes+1), admin); err == nil {
		t.Error("installed a module over the size cap")
	}
}

// --- containment: the roadmap AC ---

// hostFor builds a Host with no objects and no key source: the containment
// tests are about the sandbox, not about what a granted plugin may reach.
func hostFor(db *storage.DB, limits Limits) Host {
	return Host{DB: db, Limits: limits}
}

func TestInfiniteLoopIsKilledByTheDeadline(t *testing.T) {
	db := testSQLiteDB(t)
	tenant := newTenant(t, db)
	admin := approver(t, db, tenant)
	p := install(t, db, tenant, "id: com.acme.loop\nversion: 1.0.0\nfunctions: [run]\n",
		corpusModule(t, "loop"), admin)

	limits := Limits{Timeout: 300 * time.Millisecond}
	start := time.Now()
	_, err := Call(context.Background(), hostFor(db, limits), tenant, p, "run", nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("an infinite loop returned normally — nothing stopped it")
	}
	// The fault must actually have fired: if the module returned quickly on its
	// own the test proves nothing about containment (WP-2.3a's rule).
	if elapsed < limits.Timeout {
		t.Errorf("returned after %v, before the %v deadline — the loop did not run", elapsed, limits.Timeout)
	}
	if elapsed > 10*time.Second {
		t.Errorf("took %v to stop a runaway plugin", elapsed)
	}
	t.Logf("contained after %v: %v", elapsed, err)
}

func TestMemoryBombIsRefusedByThePageCap(t *testing.T) {
	db := testSQLiteDB(t)
	tenant := newTenant(t, db)
	admin := approver(t, db, tenant)
	p := install(t, db, tenant, "id: com.acme.bomb\nversion: 1.0.0\nfunctions: [run]\n",
		corpusModule(t, "bomb"), admin)

	// 256 pages = 16MiB, well under what the bomb tries to take.
	limits := Limits{MaxPages: 256, Timeout: 10 * time.Second}
	_, err := Call(context.Background(), hostFor(db, limits), tenant, p, "run", nil)
	if err == nil {
		t.Fatal("a memory bomb returned normally — the page cap did not hold")
	}
	t.Logf("contained: %v", err)

	// Non-vacuity: the same module under a generous cap must get *further*,
	// or this test would pass against a plugin that crashes for its own
	// reasons before allocating anything.
	if testing.Short() {
		return
	}
	generous := Limits{MaxPages: 1024, Timeout: 10 * time.Second}
	if _, err := Call(context.Background(), hostFor(db, generous), tenant, p, "run", nil); err == nil {
		t.Error("the bomb succeeded under a larger cap; it is not allocating enough to be an adversary")
	}
}

func TestUngrantedHostFunctionsCannotEvenBeImported(t *testing.T) {
	db := testSQLiteDB(t)
	tenant := newTenant(t, db)
	admin := approver(t, db, tenant)
	// A manifest asking for nothing at all: the thief module imports
	// lasterp_object_get and lasterp_secret_get regardless.
	p := install(t, db, tenant, "id: com.acme.thief\nversion: 1.0.0\nfunctions: [steal]\n",
		corpusModule(t, "thief"), admin)

	_, err := Call(context.Background(), hostFor(db, Limits{}), tenant, p, "steal", nil)
	if err == nil {
		t.Fatal("INV-X1: a plugin reached host functions its manifest never requested")
	}
	// It must fail at *instantiation*, not at the call: the host function table
	// is built from approved capabilities, so an ungranted import has nothing
	// to bind to and the module never runs an instruction.
	if !strings.Contains(err.Error(), "instantiate") {
		t.Errorf("INV-X1: expected an instantiation failure, got %v", err)
	}
	t.Logf("contained: %v", err)
}

func TestSandboxHasNoFilesystemNetworkOrEnvironment(t *testing.T) {
	db := testSQLiteDB(t)
	tenant := newTenant(t, db)
	admin := approver(t, db, tenant)
	p := install(t, db, tenant, "id: com.acme.escape\nversion: 1.0.0\nfunctions: [escape]\n",
		corpusModule(t, "escape"), admin)

	// A real environment variable, so "not set" inside the sandbox is evidence
	// of isolation rather than of an empty process.
	t.Setenv("LASTERP_DSN", "postgres://should-not-be-visible/lasterp")

	out, err := Call(context.Background(), hostFor(db, Limits{Timeout: 5 * time.Second}), tenant, p, "escape", nil)
	if err != nil {
		// A trap is also containment, but then the report below is unavailable.
		t.Logf("escape module trapped: %v", err)
		return
	}
	report := string(out)
	t.Logf("escape report: %s", report)
	for _, forbidden := range []string{"READ THE FILE", "WROTE THE FILE", "LISTED", "CONNECTED", "READ postgres"} {
		if strings.Contains(report, forbidden) {
			t.Errorf("INV-X1: the sandbox is not empty — %q in %s", forbidden, report)
		}
	}
	// Non-vacuity: the module must have actually tried all five.
	for _, attempted := range []string{"read_etc_passwd", "write_tmp", "list_root", "read_env", "dial"} {
		if !strings.Contains(report, attempted) {
			t.Errorf("the escape module did not attempt %s; the report proves less than it looks", attempted)
		}
	}
}

func TestUndeclaredFunctionIsUnreachable(t *testing.T) {
	db := testSQLiteDB(t)
	tenant := newTenant(t, db)
	admin := helloApprover(t, db, tenant)
	// The module exports echo, say, read, write, secret and chatter; the
	// manifest declares one of them.
	p := install(t, db, tenant, helloWith("echo"), corpusModule(t, "hello"), admin)

	if _, err := Call(context.Background(), hostFor(db, Limits{}), tenant, p, "chatter", nil); !errors.Is(err, ErrFunctionNotDeclared) {
		t.Errorf("calling an undeclared export = %v, want ErrFunctionNotDeclared", err)
	}
	out, err := Call(context.Background(), hostFor(db, Limits{}), tenant, p, "echo", []byte("hi"))
	if err != nil {
		t.Fatalf("declared function: %v", err)
	}
	if string(out) != "echo:hi" {
		t.Errorf("echo returned %q", out)
	}
}

func TestHostCallBudgetStopsAChattyPlugin(t *testing.T) {
	db := testSQLiteDB(t)
	tenant := newTenant(t, db)
	admin := helloApprover(t, db, tenant)
	p := install(t, db, tenant, helloWith("chatter"), corpusModule(t, "hello"), admin)

	// The stand-in for the fuel meter wazero does not have (decisions §2).
	limits := Limits{MaxHostCalls: 10, Timeout: 30 * time.Second}
	_, err := Call(context.Background(), hostFor(db, limits), tenant, p, "chatter", nil)
	if !errors.Is(err, ErrHostCallBudget) {
		t.Fatalf("chatter = %v, want ErrHostCallBudget", err)
	}
}

func TestTamperedModuleIsRefused(t *testing.T) {
	ctx := context.Background()
	db := testSQLiteDB(t)
	tenant := newTenant(t, db)
	admin := helloApprover(t, db, tenant)
	p := install(t, db, tenant, helloWith("echo"), corpusModule(t, "hello"), admin)

	// Swap the stored bytes for a different module, as a compromised database
	// or a bad restore would. The recorded hash is what an administrator
	// approved; the bytes must still match it.
	swap(t, db, tenant, p.ID, corpusModule(t, "loop"))

	if _, err := Get(ctx, db, tenant, p.ID); err == nil || !strings.Contains(err.Error(), "refusing to run bytes nobody approved") {
		t.Fatalf("Get on a tampered module = %v, want a hash refusal", err)
	}
}

// swap replaces an installed plugin's stored bytes without touching its
// recorded hash — the shape of a bad restore or a compromised row.
func swap(t *testing.T, db *storage.DB, tenant tenancy.ID, id string, module []byte) {
	t.Helper()
	err := tenancy.WithTenant(context.Background(), db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, db.Rebind(`UPDATE plugins SET module = ? WHERE tenant_id = ? AND id = ?`),
			base64.StdEncoding.EncodeToString(module), string(tenant), id)
		return err
	})
	if err != nil {
		t.Fatalf("swap module: %v", err)
	}
}
