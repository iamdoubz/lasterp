//go:build integrity

// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/iamdoubz/lasterp/kernel/idgen"
	"github.com/iamdoubz/lasterp/kernel/metadata"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// /api/v1/meta/objects is the schema surface the web client renders from. It is
// a tenant-scoped read path (overlays are per-tenant, ADR-006), so it carries
// the same INV-T1 obligation as any data route.

// The endpoint returns renderable schemas with the resource path the gateway
// actually routes on — a mismatch here means every generated client URL 404s.
func TestMetaObjectsMatchGatewayRoutes(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)

			status, body, _ := e.get("/api/v1/meta/objects")
			if status != http.StatusOK {
				t.Fatalf("meta status = %d, want 200; body=%s", status, body)
			}
			objects := decodeMetaObjects(t, body)
			if len(objects) == 0 {
				t.Fatal("no renderable objects returned")
			}

			for _, o := range objects {
				if len(o.Fields) == 0 {
					t.Errorf("object %s has no fields", o.Name)
				}
				// The advertised resource must be a live CRUD route.
				if s, b, _ := e.get("/api/v1/" + o.Resource); s != http.StatusOK {
					t.Errorf("advertised resource %q for %s = %d, want 200; body=%s", o.Resource, o.Name, s, b)
				}
			}
		})
	}
}

// A disabled module has no screens: its objects are absent from the metadata
// the client navigates by, rather than appearing and then 403ing per request.
func TestMetaObjectsExcludeDisabledModules(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)

			_, before, _ := e.get("/api/v1/meta/objects")
			if !hasObject(t, before, "Contact") {
				t.Fatal("Contact missing while contacts is enabled")
			}

			// invoicing declares contacts in its requires: closure, so contacts
			// cannot come out from under it — that refusal is a 409 with the
			// blocking modules named, not an opaque 500 (ADR-018).
			if status, body, _ := e.post("/api/v1/capabilities/contacts/disable", nil); status != http.StatusConflict {
				t.Fatalf("disable contacts while invoicing is on = %d, want 409; body=%s", status, body)
			}
			if hasObject(t, mustGet(t, e, "/api/v1/meta/objects"), "Contact") != true {
				t.Error("refused disable still removed Contact from the schema surface")
			}

			// Take the dependent down first, then the dependency.
			if status, body, _ := e.post("/api/v1/capabilities/invoicing/disable", nil); status != http.StatusOK {
				t.Fatalf("disable invoicing = %d; body=%s", status, body)
			}
			if status, body, _ := e.post("/api/v1/capabilities/contacts/disable", nil); status != http.StatusOK {
				t.Fatalf("disable contacts = %d; body=%s", status, body)
			}

			_, after, _ := e.get("/api/v1/meta/objects")
			if hasObject(t, after, "Contact") {
				t.Error("Contact still advertised after its module was disabled")
			}
		})
	}
}

// INV-T1: the schema a tenant sees is its own. Two tenants querying the same
// endpoint must not see each other's objects, and the response must never carry
// rows scoped to a tenant other than the caller's.
func TestMetaObjectsAreTenantScoped(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			// Tenant A enables every module; tenant B enables none.
			a := seed(t, db)
			b := seedBareTenant(t, db, a.server)

			_, aBody, _ := a.get("/api/v1/meta/objects")
			aObjects := decodeMetaObjects(t, aBody)
			if len(aObjects) == 0 {
				t.Fatal("tenant A sees no objects despite enabled modules")
			}

			status, bBody, _ := b.get("/api/v1/meta/objects")
			if status != http.StatusOK {
				t.Fatalf("tenant B meta status = %d, want 200; body=%s", status, bBody)
			}
			bObjects := decodeMetaObjects(t, bBody)
			if len(bObjects) != 0 {
				t.Errorf("tenant B with no enabled modules sees %d objects: %v", len(bObjects), bObjects)
			}
		})
	}
}

// seedBareTenant provisions a second tenant on an already-running server with a
// fully-granted principal but no modules enabled — the "fresh tenant" state
// (WP-1.4b-decisions.md §5) and the control side of the isolation test.
func seedBareTenant(t *testing.T, db *storage.DB, srv *httptest.Server) *env {
	t.Helper()
	tenant := tenancy.ID(idgen.New())
	if err := tenancy.CreateTenant(context.Background(), db, tenant, "isolation tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	e := &env{t: t, server: srv, db: db, tenant: tenant}
	e.token = e.issueUser(t, fullGrants())
	return e
}

func decodeMetaObjects(t *testing.T, body []byte) []metaObject {
	t.Helper()
	var resp struct {
		Data []metaObject `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode meta objects: %v (body=%s)", err, body)
	}
	return resp.Data
}

func mustGet(t *testing.T, e *env, path string) []byte {
	t.Helper()
	status, body, _ := e.get(path)
	if status != http.StatusOK {
		t.Fatalf("GET %s = %d; body=%s", path, status, body)
	}
	return body
}

func hasObject(t *testing.T, body []byte, name string) bool {
	t.Helper()
	for _, o := range decodeMetaObjects(t, body) {
		if o.Name == name {
			return true
		}
	}
	return false
}

// Every published object says how it is persisted, because a replica generating
// its schema from this endpoint has to know which objects it can hydrate:
// /api/v1/sync/snapshot 404s for event-sourced ones, and without this field the
// only ways to find out are probing a 404 (which conflates event-sourced with
// unknown and with disabled) or keeping the list client-side (a hand-written
// duplicate of metadata, which ADR-006 and CLAUDE.md both forbid).
// WP-2.2b-decisions.md §2.
func TestMetaObjectsPublishPersistence(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)
			objects := decodeMetaObjects(t, mustGet(t, e, "/api/v1/meta/objects"))
			if len(objects) == 0 {
				t.Fatal("no renderable objects returned")
			}

			for _, o := range objects {
				switch o.Persistence {
				case metadata.PersistenceCRUD, metadata.PersistenceEventSourced:
				default:
					t.Errorf("object %s publishes persistence %q, want one of %q/%q",
						o.Name, o.Persistence, metadata.PersistenceCRUD, metadata.PersistenceEventSourced)
				}

				// The claim must be true: a crud object hydrates, and anything
				// else does not. This is the assertion that keeps the field
				// honest rather than merely present.
				status, _, _ := e.get("/api/v1/sync/snapshot?object=" + o.Name + "&limit=1")
				want := http.StatusOK
				if o.Persistence != metadata.PersistenceCRUD {
					want = http.StatusNotFound
				}
				if status != want {
					t.Errorf("object %s says persistence=%q but snapshot returned %d, want %d",
						o.Name, o.Persistence, status, want)
				}
			}
		})
	}
}
