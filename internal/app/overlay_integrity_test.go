//go:build integrity

// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/iamdoubz/lasterp/kernel/capability"
	"github.com/iamdoubz/lasterp/kernel/idgen"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// WP-3.2c: tenant metadata overlays — ADR-006's customization layer, end to end
// over the wired product handler on both dialects.
//
// Invariants: **INV-T3** (an overlay may narrow an option set and raise a
// permission floor, never the reverse), **INV-T5** (every stored value conforms
// to the *effective* schema, which is now the tenant's), **INV-T1** (a tenant's
// customization is invisible to every other tenant), **INV-T4** (a schema change
// names who made it).
//
// The load-bearing property is that all three request paths read the same
// resolved schema: the CRUD route that accepts the write, the metadata endpoint
// the renderer and the bindings generator read, and the replica's snapshot. A
// custom field that reached only one of them would be a customization that looks
// like it worked.

const contactLoyaltyOverlay = `object: Contact
add_fields:
  - {name: loyalty_tier, type: enum, options: [bronze, silver, gold]}
`

// putOverlay stores the tenant's overlay for one object. The body is the YAML
// document itself, not JSON wrapping it — the same shape automations take, for
// the same reason: what is stored is the document, so what is sent should be.
func (e *env) putOverlay(object, yaml string) (int, []byte) {
	e.t.Helper()
	req, err := http.NewRequest("PUT", e.server.URL+"/api/v1/meta/overlays/"+object, strings.NewReader(yaml))
	if err != nil {
		e.t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+e.token)
	req.Header.Set("Content-Type", "application/yaml")
	req.Header.Set("Idempotency-Key", idgen.New())
	resp, err := e.server.Client().Do(req)
	if err != nil {
		e.t.Fatalf("put overlay: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

// metaFieldsFor returns the field names /api/v1/meta/objects publishes for one
// object, as this env's tenant sees them.
func metaFieldsFor(t *testing.T, e *env, object string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, o := range decodeMetaObjects(t, mustGet(t, e, "/api/v1/meta/objects")) {
		if o.Name != object {
			continue
		}
		for _, f := range o.Fields {
			out[f.Name] = f.Options
			if f.Options == nil {
				out[f.Name] = []string{}
			}
		}
	}
	return out
}

// AC1: a tenant overlay adding a field to a shipped object round-trips through
// the API, the metadata endpoint and the replica, on both dialects.
func TestTenantOverlayFieldRoundTrips(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)

			// Before: the field does not exist, and a write naming it is
			// refused. Establishing this first is what makes the "after" half
			// evidence of the overlay rather than of a permissive engine.
			if _, ok := metaFieldsFor(t, e, "Contact")["loyalty_tier"]; ok {
				t.Fatal("loyalty_tier exists before any overlay was stored")
			}

			if status, body := e.putOverlay("Contact", contactLoyaltyOverlay); status != http.StatusOK {
				t.Fatalf("PUT overlay = %d, want 200; body=%s", status, body)
			}

			// 1. The metadata endpoint publishes it, with its option set — the
			//    renderer offers exactly what the server will accept.
			fields := metaFieldsFor(t, e, "Contact")
			opts, ok := fields["loyalty_tier"]
			if !ok {
				t.Fatalf("meta/objects does not publish loyalty_tier; fields=%v", fields)
			}
			if strings.Join(opts, ",") != "bronze,silver,gold" {
				t.Fatalf("loyalty_tier options = %v, want [bronze silver gold]", opts)
			}

			// 2. The CRUD route accepts and returns it.
			status, body, rec := e.post("/api/v1/contact", map[string]any{
				"name": "Ada Lovelace", "kind": "customer", "loyalty_tier": "gold",
			})
			if status != http.StatusCreated {
				t.Fatalf("create contact = %d, want 201; body=%s", status, body)
			}
			id := mustField(t, rec, "id")
			if got := rec["loyalty_tier"]; got != "gold" {
				t.Fatalf("created loyalty_tier = %v, want gold", got)
			}
			_, getBody, got := e.get("/api/v1/contact/" + id)
			if got["loyalty_tier"] != "gold" {
				t.Fatalf("read back loyalty_tier = %v, want gold; body=%s", got["loyalty_tier"], getBody)
			}

			// 3. The replica sees it. A snapshot is how a device hydrates, so a
			//    custom field absent here is one that never reaches offline
			//    work — the half of "round-trips" a pure API test would miss.
			if !snapshotHasField(t, e, "Contact", id, "loyalty_tier", "gold") {
				t.Error("the sync snapshot does not carry the overlay field")
			}

			// And the overlay is attributable and exportable byte-for-byte
			// (INV-T4, ADR-006's "customization packages").
			stored := listOverlays(t, e)
			if len(stored) != 1 {
				t.Fatalf("listed %d overlays, want 1", len(stored))
			}
			if stored[0]["definition"] != contactLoyaltyOverlay {
				t.Errorf("definition = %q, want it stored verbatim", stored[0]["definition"])
			}
			if stored[0]["updated_by"] == "" {
				t.Error("the overlay records no actor (INV-T4)")
			}

			// Removing it removes the field, so a customization is reversible
			// rather than a one-way door.
			if status, body, _ := e.call("DELETE", "/api/v1/meta/overlays/Contact", e.token, idgen.New(), nil); status != http.StatusNoContent {
				t.Fatalf("DELETE overlay = %d, want 204; body=%s", status, body)
			}
			if _, ok := metaFieldsFor(t, e, "Contact")["loyalty_tier"]; ok {
				t.Error("loyalty_tier survived its overlay being deleted")
			}
		})
	}
}

// AC2, INV-T3: core's declaration is a bound no overlay may escape — permissions
// from below, option sets from above. Every refusal is paired with the mirrored
// call succeeding, or the test would pass just as well against a route that
// refused everything.
func TestOverlayCannotWidenWhatCoreDeclared(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)

			// Contact.kind is a core enum. Find its declared set so the test
			// narrows and widens against the real schema rather than a copy of
			// it that could drift.
			coreKinds := metaFieldsFor(t, e, "Contact")["kind"]
			if len(coreKinds) < 2 {
				t.Fatalf("Contact.kind options = %v, want at least two to narrow", coreKinds)
			}

			widen := "object: Contact\nnarrow_options:\n  kind: [" +
				strings.Join(coreKinds, ", ") + ", banana]\n"
			status, body := e.putOverlay("Contact", widen)
			if status != http.StatusUnprocessableEntity {
				t.Fatalf("widening overlay = %d, want 422; body=%s", status, body)
			}
			if !strings.Contains(string(body), "banana") {
				t.Errorf("the refusal does not name the offending value: %s", body)
			}

			narrow := "object: Contact\nnarrow_options:\n  kind: [" + coreKinds[0] + "]\n"
			if status, body := e.putOverlay("Contact", narrow); status != http.StatusOK {
				t.Fatalf("narrowing overlay = %d, want 200; body=%s", status, body)
			}

			// A permission floor may be raised and never lowered. Contact:read
			// is granted to some role at core; an overlay handing it to fewer
			// roles is the data-side of an authorization downgrade.
			lower := "object: Contact\npermissions:\n  read: [nobody.at.all]\n"
			if status, body := e.putOverlay("Contact", lower); status != http.StatusUnprocessableEntity {
				t.Fatalf("floor-lowering overlay = %d, want 422; body=%s", status, body)
			}
		})
	}
}

// AC2, INV-T5: the schema a write is validated against is the tenant's
// effective one — so a value core allows is refused once the tenant has narrowed
// it, and a value outside an overlay-added enum is refused too.
func TestWritesConformToTheTenantsEffectiveSchema(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)
			coreKinds := metaFieldsFor(t, e, "Contact")["kind"]
			if len(coreKinds) < 2 {
				t.Fatalf("Contact.kind options = %v, want at least two", coreKinds)
			}
			keep, drop := coreKinds[0], coreKinds[1]

			// Both values are legal before the narrowing — the non-vacuity
			// half, and the reason the refusal below is about the overlay.
			if status, body, _ := e.post("/api/v1/contact", map[string]any{
				"name": "before", "kind": drop,
			}); status != http.StatusCreated {
				t.Fatalf("create with %q before narrowing = %d; body=%s", drop, status, body)
			}

			narrow := "object: Contact\nnarrow_options:\n  kind: [" + keep + "]\n"
			if status, body := e.putOverlay("Contact", narrow); status != http.StatusOK {
				t.Fatalf("PUT overlay = %d; body=%s", status, body)
			}

			status, body, _ := e.post("/api/v1/contact", map[string]any{
				"name": "after", "kind": drop,
			})
			if status != http.StatusUnprocessableEntity {
				t.Fatalf("create with a narrowed-away value = %d, want 422; body=%s", status, body)
			}
			if status, body, _ := e.post("/api/v1/contact", map[string]any{
				"name": "after ok", "kind": keep,
			}); status != http.StatusCreated {
				t.Fatalf("create with a kept value = %d, want 201; body=%s", status, body)
			}

			// An overlay-added enum is validated like any other field: the
			// value lives in the custom_fields blob, which is not a bag the
			// engine waves through.
			if status, body := e.putOverlay("Contact", contactLoyaltyOverlay+"narrow_options:\n  kind: ["+keep+"]\n"); status != http.StatusOK {
				t.Fatalf("PUT overlay (add + narrow) = %d; body=%s", status, body)
			}
			if status, body, _ := e.post("/api/v1/contact", map[string]any{
				"name": "bad tier", "kind": keep, "loyalty_tier": "platinum",
			}); status != http.StatusUnprocessableEntity {
				t.Fatalf("create with a value outside an overlay enum = %d, want 422; body=%s", status, body)
			}
		})
	}
}

// AC3, INV-T1: a second tenant is unaffected. Asserted on all three surfaces,
// because the overlay store is read on each of them independently and a leak on
// any one is a leak.
func TestOverlaysDoNotCrossTenants(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			a := seed(t, db)
			b := seedTenantOnServer(t, db, a.server)

			if status, body := a.putOverlay("Contact", contactLoyaltyOverlay); status != http.StatusOK {
				t.Fatalf("tenant A PUT overlay = %d; body=%s", status, body)
			}
			if _, ok := metaFieldsFor(t, a, "Contact")["loyalty_tier"]; !ok {
				t.Fatal("tenant A does not see its own overlay")
			}

			// 1. The metadata endpoint.
			if _, ok := metaFieldsFor(t, b, "Contact")["loyalty_tier"]; ok {
				t.Error("tenant B sees tenant A's overlay field in meta/objects (INV-T1)")
			}
			// 2. The overlay list.
			if got := listOverlays(t, b); len(got) != 0 {
				t.Errorf("tenant B lists %d overlays, want 0: %v", len(got), got)
			}
			// 3. The write path — B writing the field must be refused, since
			//    for B it is not a field at all.
			if status, body, _ := b.post("/api/v1/contact", map[string]any{
				"name": "B", "kind": "customer", "loyalty_tier": "gold",
			}); status == http.StatusCreated {
				t.Errorf("tenant B stored a field only tenant A declared; body=%s", body)
			}
		})
	}
}

// An overlay naming an object this host does not serve as generic CRUD is
// refused rather than stored. Storing it would produce a customization that
// nothing could ever write — the failure mode the plugin manifest's strict parse
// exists to prevent, arriving through the other door.
func TestOverlayOnAnUnknownObjectIsRefused(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)
			if status, body := e.putOverlay("NotAThing", "object: NotAThing\nadd_fields:\n  - {name: x, type: bool}\n"); status != http.StatusNotFound {
				t.Fatalf("overlay on an unknown object = %d, want 404; body=%s", status, body)
			}
			// Invoice is a real object the plugin host can read, and is
			// deliberately not customizable: its writes go through invoicing's
			// posting pipeline, which builds its own schema (INV-F5).
			if status, body := e.putOverlay("Invoice", "object: Invoice\nadd_fields:\n  - {name: x, type: bool}\n"); status != http.StatusNotFound {
				t.Fatalf("overlay on Invoice = %d, want 404; body=%s", status, body)
			}
		})
	}
}

// A field name is input now, not repo-authored YAML. Anything but a lower
// snake_case identifier is refused, because a field name is interpolated into
// SQL as a column name wherever one is needed.
func TestOverlayFieldNamesAreIdentifiers(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)
			for _, bad := range []string{
				`x"); DROP TABLE obj_contact; --`,
				"Loyalty-Tier",
				"1st_tier",
			} {
				yaml := "object: Contact\nadd_fields:\n  - {name: '" + strings.ReplaceAll(bad, "'", "''") + "', type: bool}\n"
				if status, body := e.putOverlay("Contact", yaml); status != http.StatusUnprocessableEntity {
					t.Errorf("field name %q = %d, want 422; body=%s", bad, status, body)
				}
			}
			// Non-vacuity: a well-formed name on the same route is accepted.
			if status, body := e.putOverlay("Contact", "object: Contact\nadd_fields:\n  - {name: loyalty_tier2, type: bool}\n"); status != http.StatusOK {
				t.Fatalf("a valid field name = %d, want 200; body=%s", status, body)
			}
		})
	}
}

// --- helpers ---

// seedTenantOnServer provisions a second fully-granted tenant with the same
// modules enabled, on an already-running server. seedBareTenant is the
// no-modules variant; an isolation test for schemas needs the modules on, or
// tenant B would see no objects for a reason unrelated to overlays.
func seedTenantOnServer(t *testing.T, db *storage.DB, srv *httptest.Server) *env {
	t.Helper()
	tenant := tenancy.ID(idgen.New())
	if err := tenancy.CreateTenant(context.Background(), db, tenant, "overlay isolation tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	e := &env{t: t, server: srv, db: db, tenant: tenant}
	reg, err := capability.Load()
	if err != nil {
		t.Fatalf("capability.Load: %v", err)
	}
	adminCtx := e.actorCtx(t, map[string][]string{"capability": {"manage"}})
	for _, module := range []string{"contacts", "ledger", "tax-engine", "invoicing"} {
		if _, err := capability.Enable(adminCtx, db, reg, tenant, module); err != nil {
			t.Fatalf("enable %s: %v", module, err)
		}
	}
	e.token = e.issueUser(t, fullGrants())
	return e
}

func listOverlays(t *testing.T, e *env) []map[string]string {
	t.Helper()
	var resp struct {
		Overlays []map[string]any `json:"overlays"`
	}
	if err := json.Unmarshal(mustGet(t, e, "/api/v1/meta/overlays"), &resp); err != nil {
		t.Fatalf("decode overlays: %v", err)
	}
	out := make([]map[string]string, 0, len(resp.Overlays))
	for _, o := range resp.Overlays {
		row := map[string]string{}
		for k, v := range o {
			if s, ok := v.(string); ok {
				row[k] = s
			}
		}
		out = append(out, row)
	}
	return out
}

// snapshotHasField pages the replica-hydration endpoint for one object and
// reports whether the named row carries field == want.
func snapshotHasField(t *testing.T, e *env, object, id, field, want string) bool {
	t.Helper()
	status, body, parsed := e.get("/api/v1/sync/snapshot?object=" + object + "&limit=100")
	if status != http.StatusOK {
		t.Fatalf("snapshot = %d; body=%s", status, body)
	}
	rows, _ := parsed["data"].([]any)
	for _, r := range rows {
		row, _ := r.(map[string]any)
		if row["id"] != id {
			continue
		}
		return row[field] == want
	}
	return false
}
