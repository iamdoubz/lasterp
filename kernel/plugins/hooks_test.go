// SPDX-License-Identifier: AGPL-3.0-only

package plugins

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/iamdoubz/lasterp/kernel/authz"
	"github.com/iamdoubz/lasterp/kernel/metadata"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// WP-3.1b: the hook surface. Invariants: **INV-X2** (plugin failure never
// corrupts or partially commits a transaction — structural here, see
// TestNoHookRunsInsideATransaction), INV-T3 (a hook may narrow a write, never
// widen its authority), INV-T4 (breaker transitions are attributable), INV-T5
// (a hook's enrichment is re-validated), INV-T1 (kv is tenant- and
// plugin-scoped).

const hooksManifest = `
id: com.acme.hooks
version: 1.0.0
functions: [veto, enrich, smuggle, boom, note, spawn]
capabilities:
  objects:
    - {type: Widget, access: read}
    - {type: Widget, access: write}
hooks:
  - {event: Widget.before_create, fn: veto, mode: sync}
`

func hooksManifestWith(hooks string) string {
	return `
id: com.acme.hooks
version: 1.0.0
functions: [veto, enrich, smuggle, boom, note, spawn]
capabilities:
  objects:
    - {type: Widget, access: read}
    - {type: Widget, access: write}
hooks:
` + hooks
}

func hooksApprover(t *testing.T, db *storage.DB, tenant tenancy.ID) authz.Actor {
	t.Helper()
	return approver(t, db, tenant,
		[2]string{"Widget", "read"}, [2]string{"Widget", "create"}, [2]string{"Widget", "update"})
}

// installHooks installs the hook corpus member with the given hooks block.
func installHooks(t *testing.T, db *storage.DB, tenant tenancy.ID, hooks string) *Installed {
	t.Helper()
	return install(t, db, tenant, hooksManifestWith(hooks), corpusModule(t, "hooks"),
		hooksApprover(t, db, tenant))
}

// --- manifest ---

func TestHookManifestValidation(t *testing.T) {
	cases := map[string]string{
		"unknown mode":     "  - {event: Widget.before_create, fn: veto, mode: whenever}\n",
		"unknown fn":       "  - {event: Widget.before_create, fn: nope, mode: sync}\n",
		"bad event shape":  "  - {event: before_create, fn: veto, mode: sync}\n",
		"async on a verb":  "  - {event: Widget.before_create, fn: note, mode: async}\n",
		"sync on changed":  "  - {event: Widget.changed, fn: veto, mode: sync}\n",
		"bad on_failure":   "  - {event: Widget.before_create, fn: veto, mode: sync, on_failure: maybe}\n",
		"timeout too high": "  - {event: Widget.before_create, fn: veto, mode: sync, timeout_ms: 5000}\n",
	}
	for name, hooks := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseManifest([]byte(hooksManifestWith(hooks))); err == nil {
				t.Error("accepted a hook declaration this host cannot honour")
			}
		})
	}

	// `Invoice.posted` is the docs/05 example, and this host cannot honour it:
	// the feed records that an object changed, not what was done to it. Refused
	// by name rather than silently delivering on every change (decisions §2).
	_, err := ParseManifest([]byte(hooksManifestWith("  - {event: Widget.posted, fn: note, mode: async}\n")))
	if err == nil || !strings.Contains(err.Error(), "Widget.changed") {
		t.Errorf("a verb-shaped async event must be refused with the supported form, got %v", err)
	}

	m, err := ParseManifest([]byte(hooksManifest))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if got := m.SyncHooks("Widget", metadata.VerbCreate); len(got) != 1 || got[0].Fn != "veto" {
		t.Errorf("SyncHooks = %v", got)
	}
	if got := m.SyncHooks("Widget", metadata.VerbUpdate); len(got) != 0 {
		t.Errorf("a before_create hook fired on update: %v", got)
	}
	// The default budget is the one that keeps docs/09's write promise intact.
	if got := m.Hooks[0].Timeout(); got != DefaultHookTimeoutMS*time.Millisecond {
		t.Errorf("default hook timeout = %v", got)
	}
	if !m.Hooks[0].FailsClosed() {
		t.Error("a hook must fail closed unless it says otherwise")
	}
	if w := m.Hooks[0].LatencyWarning(); !strings.Contains(w, "every write of Widget") {
		t.Errorf("latency warning does not state the cost per write: %q", w)
	}
}

// --- INV-X2, structurally ---

// txSpy is a Hooks implementation that reports whether it was called while a
// transaction was open on the connection it can see.
type txSpy struct {
	called bool
	inTx   bool
	db     *storage.DB
}

func (s *txSpy) Before(ctx context.Context, tenant tenancy.ID, object, verb string, rec metadata.Record) (metadata.Record, error) {
	s.called = true
	// SQLite serialises writers: if a write transaction were open on this
	// connection pool when the hook ran, this write would block or fail rather
	// than complete. It completing is the evidence that no transaction is held
	// across hook dispatch.
	done := make(chan error, 1)
	go func() {
		done <- tenancy.WithTenant(context.Background(), s.db, tenant, func(ctx context.Context, tx *txType) error {
			_, err := tx.ExecContext(ctx, s.db.Rebind(
				`INSERT INTO plugin_kv (tenant_id, plugin_id, key, value, updated_at) VALUES (?, ?, ?, ?, ?)`),
				string(tenant), "tx-probe", "probe", "1", time.Now().UTC())
			return err
		})
	}()
	select {
	case err := <-done:
		s.inTx = err != nil
	case <-time.After(3 * time.Second):
		// Blocked: something is holding the write lock, which is exactly what
		// INV-X2 says must not happen around a hook.
		s.inTx = true
	}
	return rec, nil
}

// TestNoHookRunsInsideATransaction is INV-X2's enforcement, in the shape
// INV-S2 uses: the invariant holds because of *where* dispatch is, not because
// of a runtime check that might miss a case. If a hook ran inside the write's
// transaction, an independent write from another goroutine could not complete
// while it ran.
func TestNoHookRunsInsideATransaction(t *testing.T) {
	db := testSQLiteDB(t)
	tenant := newTenant(t, db)
	crud := widgetCRUD(t, db)
	spy := &txSpy{db: db}
	hooked := crud.WithHooks(spy)

	ctx := actorCtx(t, db, tenant, [2]string{"Widget", "create"})
	if _, err := hooked.Create(ctx, db, tenant, metadata.Record{"name": "In A Transaction?", "kind": "customer"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !spy.called {
		t.Fatal("the hook never ran; this test proves nothing")
	}
	if spy.inTx {
		t.Error("INV-X2: a hook ran while a transaction was held — a plugin can now stall or partially commit a write")
	}
}

// TestAFailingHookLeavesNoRow is the other half: a hook that rejects must leave
// nothing behind, and one that fails must not have written half a record.
func TestAFailingHookLeavesNoRow(t *testing.T) {
	for name, db := range testDialects(t) {
		t.Run(name, func(t *testing.T) {
			tenant := newTenant(t, db)
			p := installHooks(t, db, tenant, "  - {event: Widget.before_create, fn: veto, mode: sync}\n")
			crud := widgetCRUD(t, db).WithHooks(dispatcherFor(t, db, p))
			ctx := actorCtx(t, db, tenant, [2]string{"Widget", "create"}, [2]string{"Widget", "read"})

			_, err := crud.Create(ctx, db, tenant, metadata.Record{"name": "REJECT ME", "kind": "customer"})
			if !errors.Is(err, ErrHookRejected) {
				t.Fatalf("Create = %v, want ErrHookRejected", err)
			}
			if !strings.Contains(err.Error(), "com.acme.hooks") {
				t.Errorf("a veto must name the plugin that refused: %v", err)
			}
			rows, err := crud.List(ctx, db, tenant)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if len(rows) != 0 {
				t.Errorf("INV-X2: a rejected write left %d rows behind", len(rows))
			}
		})
	}
}

// --- veto, enrichment and the re-validation that bounds it ---

func TestHookEnrichmentIsAppliedAndRevalidated(t *testing.T) {
	db := testSQLiteDB(t)
	tenant := newTenant(t, db)
	p := installHooks(t, db, tenant, "  - {event: Widget.before_create, fn: enrich, mode: sync}\n")
	crud := widgetCRUD(t, db).WithHooks(dispatcherFor(t, db, p))
	ctx := actorCtx(t, db, tenant, [2]string{"Widget", "create"})

	rec, err := crud.Create(ctx, db, tenant, metadata.Record{"name": "Plain", "kind": "customer"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got, _ := rec["name"].(string); got != "Plain (enriched)" {
		t.Errorf("enrichment did not reach the stored record: %q", got)
	}
}

func TestHookCannotSmuggleAValueTheSchemaForbids(t *testing.T) {
	db := testSQLiteDB(t)
	tenant := newTenant(t, db)
	p := installHooks(t, db, tenant, "  - {event: Widget.before_create, fn: smuggle, mode: sync}\n")
	crud := widgetCRUD(t, db).WithHooks(dispatcherFor(t, db, p))
	ctx := actorCtx(t, db, tenant, [2]string{"Widget", "create"})

	_, err := crud.Create(ctx, db, tenant, metadata.Record{"name": "Fine", "kind": "customer"})
	if !errors.Is(err, metadata.ErrValidation) {
		t.Fatalf("INV-T5: a hook wrote an out-of-set value; Create = %v, want a validation failure", err)
	}
}

// --- fail-closed by default, and the breaker's one rule ---

func TestBrokenHookRejectsByDefault(t *testing.T) {
	db := testSQLiteDB(t)
	tenant := newTenant(t, db)
	p := installHooks(t, db, tenant, "  - {event: Widget.before_create, fn: boom, mode: sync}\n")
	crud := widgetCRUD(t, db).WithHooks(dispatcherFor(t, db, p))
	ctx := actorCtx(t, db, tenant, [2]string{"Widget", "create"})

	_, err := crud.Create(ctx, db, tenant, metadata.Record{"name": "Anything", "kind": "customer"})
	if !errors.Is(err, ErrHookRejected) {
		t.Fatalf("a broken hook = %v, want the write rejected", err)
	}
	if !strings.Contains(err.Error(), "com.acme.hooks") {
		t.Errorf("the refusal must name the plugin: %v", err)
	}
}

func TestBrokenHookDeclaredAllowLetsTheWriteThrough(t *testing.T) {
	db := testSQLiteDB(t)
	tenant := newTenant(t, db)
	p := installHooks(t, db, tenant,
		"  - {event: Widget.before_create, fn: boom, mode: sync, on_failure: allow}\n")
	crud := widgetCRUD(t, db).WithHooks(dispatcherFor(t, db, p))
	ctx := actorCtx(t, db, tenant, [2]string{"Widget", "create"})

	if _, err := crud.Create(ctx, db, tenant, metadata.Record{"name": "Enrichment Failed", "kind": "customer"}); err != nil {
		t.Fatalf("an on_failure: allow hook took the write down: %v", err)
	}
}

// TestBreakerNeverSkipsAFailClosedHook is decisions §3 as a test: once a
// breaker is open, a skippable hook is skipped and a fail-closed one keeps
// rejecting. Skipping the latter would turn a rule that must hold into one that
// stops holding exactly when its plugin is misbehaving.
func TestBreakerNeverSkipsAFailClosedHook(t *testing.T) {
	db := testSQLiteDB(t)
	tenant := newTenant(t, db)
	p := installHooks(t, db, tenant, "  - {event: Widget.before_create, fn: boom, mode: sync}\n")
	d := dispatcherFor(t, db, p)
	crud := widgetCRUD(t, db).WithHooks(d)
	ctx := actorCtx(t, db, tenant, [2]string{"Widget", "create"})

	for i := 0; i < BreakerThreshold+2; i++ {
		if _, err := crud.Create(ctx, db, tenant, metadata.Record{"name": "Try", "kind": "customer"}); !errors.Is(err, ErrHookRejected) {
			t.Fatalf("write %d = %v, want a rejection even with the breaker open", i, err)
		}
		d.Forget(tenant)
	}

	reloaded, err := Get(context.Background(), db, tenant, p.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !reloaded.BreakerOpen() {
		t.Fatalf("the breaker did not open after %d failures (failures=%d)", BreakerThreshold+2, reloaded.HookFailures)
	}
	// Breaker state is on the row, so it survives the restart a bad plugin may
	// itself be causing (decisions §4).
	if reloaded.HookFailures < BreakerThreshold {
		t.Errorf("failures = %d, want at least %d", reloaded.HookFailures, BreakerThreshold)
	}
}

func TestOpenBreakerSkipsASkippableHook(t *testing.T) {
	db := testSQLiteDB(t)
	tenant := newTenant(t, db)
	p := installHooks(t, db, tenant,
		"  - {event: Widget.before_create, fn: boom, mode: sync, on_failure: allow}\n")
	d := dispatcherFor(t, db, p)
	crud := widgetCRUD(t, db).WithHooks(d)
	ctx := actorCtx(t, db, tenant, [2]string{"Widget", "create"})

	for i := 0; i < BreakerThreshold+1; i++ {
		if _, err := crud.Create(ctx, db, tenant, metadata.Record{"name": "Try", "kind": "customer"}); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		d.Forget(tenant)
	}
	stats := d.Stats().For(p.ID)
	if len(stats) == 0 {
		t.Fatal("no hook stats recorded")
	}
	if stats[0].Skipped == 0 {
		t.Error("an open breaker never skipped a skippable hook; it kept paying for a call it knew would fail")
	}
	// And the cost is attributable, which is the point of measuring at all.
	if stats[0].Plugin != p.ID {
		t.Errorf("stats are not attributed to the plugin: %+v", stats[0])
	}
}

func TestResetBreakerClosesItAndIsAudited(t *testing.T) {
	ctx := context.Background()
	db := testSQLiteDB(t)
	tenant := newTenant(t, db)
	p := installHooks(t, db, tenant, "  - {event: Widget.before_create, fn: boom, mode: sync}\n")
	d := dispatcherFor(t, db, p)
	crud := widgetCRUD(t, db).WithHooks(d)
	writeCtx := actorCtx(t, db, tenant, [2]string{"Widget", "create"})

	for i := 0; i < BreakerThreshold; i++ {
		_, _ = crud.Create(writeCtx, db, tenant, metadata.Record{"name": "Try", "kind": "customer"})
		d.Forget(tenant)
	}
	if err := ResetBreaker(ctx, db, tenant, p.ID, "admin-under-test"); err != nil {
		t.Fatalf("ResetBreaker: %v", err)
	}
	reloaded, err := Get(ctx, db, tenant, p.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if reloaded.BreakerOpen() || reloaded.HookFailures != 0 {
		t.Errorf("breaker not closed: open=%v failures=%d", reloaded.BreakerOpen(), reloaded.HookFailures)
	}
	var sawReset bool
	for _, row := range pluginAudit(t, db, tenant, p.ID) {
		if row["action"] == "breaker-reset" && row["actor"] == "admin-under-test" {
			sawReset = true
		}
	}
	if !sawReset {
		t.Error("INV-T4: closing a breaker was not attributable")
	}
}

// --- kv ---

func TestKVIsTenantAndPluginScoped(t *testing.T) {
	for name, db := range testDialects(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			a, b := newTenant(t, db), newTenant(t, db)

			if err := kvSet(ctx, db, a, "com.acme.one", "k", "tenant-a-value"); err != nil {
				t.Fatalf("kvSet: %v", err)
			}
			// Another tenant, same plugin id, same key.
			if v, found, err := kvGet(ctx, db, b, "com.acme.one", "k"); err != nil || found {
				t.Errorf("INV-T1: another tenant read %q (found=%v, err=%v)", v, found, err)
			}
			// Same tenant, another plugin.
			if v, found, err := kvGet(ctx, db, a, "com.acme.two", "k"); err != nil || found {
				t.Errorf("another plugin read %q (found=%v, err=%v)", v, found, err)
			}
			if v, found, err := kvGet(ctx, db, a, "com.acme.one", "k"); err != nil || !found || v != "tenant-a-value" {
				t.Errorf("owner read back %q (found=%v, err=%v)", v, found, err)
			}
			// Empty value deletes.
			if err := kvSet(ctx, db, a, "com.acme.one", "k", ""); err != nil {
				t.Fatalf("kvSet (delete): %v", err)
			}
			if _, found, _ := kvGet(ctx, db, a, "com.acme.one", "k"); found {
				t.Error("an empty value did not delete the key")
			}
		})
	}
}

func TestKVRefusesOversizeKeysAndValues(t *testing.T) {
	ctx := context.Background()
	db := testSQLiteDB(t)
	tenant := newTenant(t, db)
	if err := kvSet(ctx, db, tenant, "p", strings.Repeat("k", MaxKVKeyBytes+1), "v"); err == nil {
		t.Error("accepted an oversize key")
	}
	if err := kvSet(ctx, db, tenant, "p", "k", strings.Repeat("v", MaxKVValueBytes+1)); err == nil {
		t.Error("accepted an oversize value")
	}
}

// --- test fixtures ---

// txType is *sql.Tx, aliased so the transaction probe above reads as a probe
// rather than as a database/sql tutorial.
type txType = sql.Tx

// widgetSchema is a minimal CRUD object for the hook tests: one free-text field
// and one enum, which is what a hook needs to enrich something legally and to
// try to smuggle something illegal.
func widgetSchema(t *testing.T) *metadata.EffectiveSchema {
	t.Helper()
	eff, err := metadata.Merge(&metadata.Object{
		ObjectName:  "Widget",
		Module:      "plugin-tests",
		Persistence: metadata.PersistenceCRUD,
		Fields: []metadata.Field{
			{Name: "name", Type: metadata.FieldText, Required: true},
			{Name: "kind", Type: metadata.FieldEnum, Required: true, Options: []string{"customer", "supplier"}},
		},
	})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	return eff
}

// applyWidgetDDL creates the Widget table. It runs from the test harness on a
// privileged connection, never from a test body: on Postgres the tests connect
// as the restricted app role, which deliberately cannot CREATE TABLE.
func applyWidgetDDL(t *testing.T, db *storage.DB) {
	t.Helper()
	if err := metadata.ApplyDDL(context.Background(), db, widgetSchema(t), 1); err != nil {
		t.Fatalf("ApplyDDL: %v", err)
	}
}

// widgetCRUD returns a CRUD engine over the Widget table the harness created.
func widgetCRUD(t *testing.T, _ *storage.DB) *metadata.CRUD {
	t.Helper()
	crud, err := metadata.NewCRUD(widgetSchema(t))
	if err != nil {
		t.Fatalf("NewCRUD: %v", err)
	}
	return crud
}

// actorCtx returns a context bound to a principal holding grants.
func actorCtx(t *testing.T, db *storage.DB, tenant tenancy.ID, grants ...[2]string) context.Context {
	t.Helper()
	return authz.WithActor(context.Background(), approver(t, db, tenant, grants...))
}

// dispatcherFor builds a dispatcher over one installed plugin's tenant.
func dispatcherFor(t *testing.T, db *storage.DB, _ *Installed) *Dispatcher {
	t.Helper()
	return NewDispatcher(Host{
		DB:      db,
		Objects: map[string]*metadata.CRUD{"Widget": widgetCRUD(t, db)},
		Limits:  Limits{MaxPages: 1024, Timeout: 2 * time.Second, MaxHostCalls: 1000},
	})
}

// pluginAudit returns the audit rows for a plugin.
func pluginAudit(t *testing.T, db *storage.DB, tenant tenancy.ID, id string) []map[string]string {
	t.Helper()
	var out []map[string]string
	err := tenancy.WithTenant(context.Background(), db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, db.Rebind(
			`SELECT action, actor_id FROM audit_log WHERE tenant_id = ? AND object = 'plugin' AND record_id = ? ORDER BY at, id`),
			string(tenant), id)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		var list []map[string]string
		for rows.Next() {
			var action, actor string
			if err := rows.Scan(&action, &actor); err != nil {
				return err
			}
			list = append(list, map[string]string{"action": action, "actor": actor})
		}
		if err := rows.Err(); err != nil {
			return err
		}
		out = list
		return nil
	})
	if err != nil {
		t.Fatalf("read plugin audit: %v", err)
	}
	return out
}
