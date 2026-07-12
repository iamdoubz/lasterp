// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/iamdoubz/lasterp/kernel/authz"
	"github.com/iamdoubz/lasterp/kernel/metadata"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// stubCaps is a CapabilityChecker returning a fixed answer, so the gateway's
// capability gate can be tested without the capability package.
type stubCaps struct {
	enabled bool
	capName string
}

func (s stubCaps) Enabled(context.Context, tenancy.ID, string) (bool, string, error) {
	return s.enabled, s.capName, nil
}

// ADR-018 §5: a request to an object whose module is disabled gets a
// capability-disabled problem+json (not a confusing 403/404), and an enabled
// module passes through.
func TestCapabilityGate(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			tenant := mustCreateTenant(t, db)
			actor := seedActor(t, db, tenant, "read")
			schema := contactSchema(t, db)

			disabled := NewGateway(Config{DB: db, Objects: []*metadata.EffectiveSchema{schema},
				Authenticator: fixedAuth(actor, tenant), Capabilities: stubCaps{enabled: false, capName: "crm"}})
			rr := do(t, disabled, http.MethodGet, "/api/v1/contact", "", nil)
			if rr.Code != http.StatusForbidden {
				t.Fatalf("disabled status = %d, want 403; body=%s", rr.Code, rr.Body)
			}
			if ct := rr.Header().Get("Content-Type"); ct != "application/problem+json" {
				t.Errorf("content-type = %q, want application/problem+json", ct)
			}
			var p Problem
			mustJSON(t, rr, &p)
			if p.Type != "capability-disabled" {
				t.Errorf("problem type = %q, want capability-disabled", p.Type)
			}

			enabled := NewGateway(Config{DB: db, Objects: []*metadata.EffectiveSchema{schema},
				Authenticator: fixedAuth(actor, tenant), Capabilities: stubCaps{enabled: true}})
			if rr := do(t, enabled, http.MethodGet, "/api/v1/contact", "", nil); rr.Code != http.StatusOK {
				t.Fatalf("enabled status = %d, want 200; body=%s", rr.Code, rr.Body)
			}
		})
	}
}

// newTestGateway builds a gateway wired to db with the Contact object and an
// authenticator presenting actor for tenant.
func newTestGateway(t *testing.T, db *storage.DB, actor authz.Actor, tenant tenancy.ID) *Gateway {
	t.Helper()
	return NewGateway(Config{
		DB:            db,
		Objects:       []*metadata.EffectiveSchema{contactSchema(t, db)},
		Authenticator: fixedAuth(actor, tenant),
	})
}

func do(t *testing.T, g *Gateway, method, path, idemKey string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		r = httptest.NewRequest(method, path, bytes.NewReader(b))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	if idemKey != "" {
		r.Header.Set("Idempotency-Key", idemKey)
	}
	rr := httptest.NewRecorder()
	g.ServeHTTP(rr, r)
	return rr
}

// TestGatewayCRUDRoundTrip exercises create/get/list/update/delete routed
// entirely from object metadata.
func TestGatewayCRUDRoundTrip(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			tenant := mustCreateTenant(t, db)
			actor := seedActor(t, db, tenant, "create", "read", "update", "delete")
			g := newTestGateway(t, db, actor, tenant)

			// Create
			rr := do(t, g, http.MethodPost, "/api/v1/contact", "k-create", map[string]any{
				"full_name": "Ada Lovelace", "email": "ada@example.com", "newsletter_opt_in": true,
			})
			if rr.Code != http.StatusCreated {
				t.Fatalf("create status = %d, want 201; body=%s", rr.Code, rr.Body)
			}
			var created map[string]any
			mustJSON(t, rr, &created)
			id, _ := created["id"].(string)
			if id == "" {
				t.Fatal("create did not return an id")
			}

			// Get
			rr = do(t, g, http.MethodGet, "/api/v1/contact/"+id, "", nil)
			if rr.Code != http.StatusOK {
				t.Fatalf("get status = %d, want 200; body=%s", rr.Code, rr.Body)
			}
			var got map[string]any
			mustJSON(t, rr, &got)
			if got["full_name"] != "Ada Lovelace" {
				t.Fatalf("full_name = %v, want Ada Lovelace", got["full_name"])
			}

			// List
			rr = do(t, g, http.MethodGet, "/api/v1/contact", "", nil)
			if rr.Code != http.StatusOK {
				t.Fatalf("list status = %d, want 200", rr.Code)
			}
			var list struct {
				Data []map[string]any `json:"data"`
			}
			mustJSON(t, rr, &list)
			if len(list.Data) != 1 {
				t.Fatalf("list len = %d, want 1", len(list.Data))
			}

			// Update
			rr = do(t, g, http.MethodPatch, "/api/v1/contact/"+id, "k-update", map[string]any{"full_name": "Ada B. Lovelace"})
			if rr.Code != http.StatusOK {
				t.Fatalf("update status = %d, want 200; body=%s", rr.Code, rr.Body)
			}

			// Delete
			rr = do(t, g, http.MethodDelete, "/api/v1/contact/"+id, "k-delete", nil)
			if rr.Code != http.StatusNoContent {
				t.Fatalf("delete status = %d, want 204", rr.Code)
			}

			// Gone from list
			rr = do(t, g, http.MethodGet, "/api/v1/contact", "", nil)
			mustJSON(t, rr, &list)
			if len(list.Data) != 0 {
				t.Fatalf("list after delete len = %d, want 0", len(list.Data))
			}
		})
	}
}

// TestIdempotentReplayReturnsIdenticalResult is the WP-0.6 AC: replaying a
// write with the same Idempotency-Key returns the identical result and does
// not create a second record.
func TestIdempotentReplayReturnsIdenticalResult(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			tenant := mustCreateTenant(t, db)
			actor := seedActor(t, db, tenant, "create", "read")
			g := newTestGateway(t, db, actor, tenant)
			body := map[string]any{"full_name": "Grace Hopper", "email": "grace@example.com"}

			first := do(t, g, http.MethodPost, "/api/v1/contact", "idem-1", body)
			if first.Code != http.StatusCreated {
				t.Fatalf("first create status = %d, want 201; body=%s", first.Code, first.Body)
			}
			second := do(t, g, http.MethodPost, "/api/v1/contact", "idem-1", body)
			if second.Code != http.StatusCreated {
				t.Fatalf("replay status = %d, want 201", second.Code)
			}
			if first.Body.String() != second.Body.String() {
				t.Fatalf("replay body differs:\n first=%s\nsecond=%s", first.Body, second.Body)
			}
			if second.Header().Get("Idempotent-Replayed") != "true" {
				t.Fatal("replay missing Idempotent-Replayed header")
			}

			// Exactly one record was created.
			list := do(t, g, http.MethodGet, "/api/v1/contact", "", nil)
			var out struct {
				Data []map[string]any `json:"data"`
			}
			mustJSON(t, list, &out)
			if len(out.Data) != 1 {
				t.Fatalf("record count = %d, want 1 (replay must not double-insert)", len(out.Data))
			}
		})
	}
}

func TestWriteRequiresIdempotencyKey(t *testing.T) {
	db := testSQLiteDB(t)
	tenant := mustCreateTenant(t, db)
	actor := seedActor(t, db, tenant, "create")
	g := newTestGateway(t, db, actor, tenant)

	rr := do(t, g, http.MethodPost, "/api/v1/contact", "", map[string]any{"full_name": "No Key"})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	assertProblem(t, rr)
}

func TestIdempotencyKeyConflictOnDifferentBody(t *testing.T) {
	db := testSQLiteDB(t)
	tenant := mustCreateTenant(t, db)
	actor := seedActor(t, db, tenant, "create")
	g := newTestGateway(t, db, actor, tenant)

	if rr := do(t, g, http.MethodPost, "/api/v1/contact", "reuse", map[string]any{"full_name": "A"}); rr.Code != http.StatusCreated {
		t.Fatalf("first status = %d, want 201; body=%s", rr.Code, rr.Body)
	}
	rr := do(t, g, http.MethodPost, "/api/v1/contact", "reuse", map[string]any{"full_name": "B"})
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rr.Code)
	}
	assertProblem(t, rr)
}

// TestFailedWriteReleasesIdempotencyKey: a validation failure must not
// consume the key (the client can retry a corrected request).
func TestFailedWriteReleasesIdempotencyKey(t *testing.T) {
	db := testSQLiteDB(t)
	tenant := mustCreateTenant(t, db)
	actor := seedActor(t, db, tenant, "create")
	g := newTestGateway(t, db, actor, tenant)

	// Missing required full_name -> 422.
	rr := do(t, g, http.MethodPost, "/api/v1/contact", "retry-key", map[string]any{"email": "x@example.com"})
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rr.Code, rr.Body)
	}
	assertProblem(t, rr)
	// Same key now works with a valid body.
	rr = do(t, g, http.MethodPost, "/api/v1/contact", "retry-key", map[string]any{"full_name": "Fixed"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("retry status = %d, want 201; body=%s", rr.Code, rr.Body)
	}
}

func TestProblemJSONForNotFoundAndForbidden(t *testing.T) {
	db := testSQLiteDB(t)
	tenant := mustCreateTenant(t, db)

	// Reader-only actor: create must be forbidden (403).
	reader := seedActor(t, db, tenant, "read")
	g := newTestGateway(t, db, reader, tenant)

	rr := do(t, g, http.MethodPost, "/api/v1/contact", "k1", map[string]any{"full_name": "X"})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("forbidden status = %d, want 403", rr.Code)
	}
	assertProblem(t, rr)

	// Unknown id -> 404.
	rr = do(t, g, http.MethodGet, "/api/v1/contact/does-not-exist", "", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("not-found status = %d, want 404; body=%s", rr.Code, rr.Body)
	}
	assertProblem(t, rr)

	// Unknown route -> 404 problem+json.
	rr = do(t, g, http.MethodGet, "/api/v1/nope", "", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown route status = %d, want 404", rr.Code)
	}
	assertProblem(t, rr)
}

func TestUnauthenticatedIsRejected(t *testing.T) {
	db := testSQLiteDB(t)
	tenant := mustCreateTenant(t, db)
	// Gateway with no authenticator: every CRUD route fails closed.
	g := NewGateway(Config{DB: db, Objects: []*metadata.EffectiveSchema{contactSchema(t, db)}})

	rr := do(t, g, http.MethodGet, "/api/v1/contact", "", nil)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
	assertProblem(t, rr)
	_ = tenant
}

// TestTenantMismatchRejected: an Authenticator returning an actor whose
// TenantID diverges from the returned tenant must be rejected (403), never
// authorized against one tenant and written to another (INV-T1/INV-T2).
func TestTenantMismatchRejected(t *testing.T) {
	db := testSQLiteDB(t)
	tenant := mustCreateTenant(t, db)
	actor := seedActor(t, db, tenant, "create", "read")
	other := tenancy.ID("some-other-tenant")

	g := NewGateway(Config{
		DB:      db,
		Objects: []*metadata.EffectiveSchema{contactSchema(t, db)},
		Authenticator: AuthenticatorFunc(func(_ *http.Request) (authz.Actor, tenancy.ID, error) {
			return actor, other, nil // actor.TenantID != returned tenant
		}),
	})

	rr := do(t, g, http.MethodGet, "/api/v1/contact", "", nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("read status = %d, want 403", rr.Code)
	}
	assertProblem(t, rr)

	rr = do(t, g, http.MethodPost, "/api/v1/contact", "k-mismatch", map[string]any{"full_name": "X"})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("write status = %d, want 403", rr.Code)
	}
	assertProblem(t, rr)
}

func TestRateLimitReturns429(t *testing.T) {
	db := testSQLiteDB(t)
	tenant := mustCreateTenant(t, db)
	actor := seedActor(t, db, tenant, "read")
	g := NewGateway(Config{
		DB:            db,
		Objects:       []*metadata.EffectiveSchema{contactSchema(t, db)},
		Authenticator: fixedAuth(actor, tenant),
		RateLimit:     RateLimit{RequestsPerSecond: 1, Burst: 2},
	})

	// Burst of 2 succeeds, the 3rd is throttled.
	for i := 0; i < 2; i++ {
		if rr := do(t, g, http.MethodGet, "/api/v1/contact", "", nil); rr.Code != http.StatusOK {
			t.Fatalf("request %d status = %d, want 200", i, rr.Code)
		}
	}
	rr := do(t, g, http.MethodGet, "/api/v1/contact", "", nil)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("throttled status = %d, want 429", rr.Code)
	}
	if rr.Header().Get("RateLimit-Limit") != "2" {
		t.Fatalf("RateLimit-Limit = %q, want 2", rr.Header().Get("RateLimit-Limit"))
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Fatal("missing Retry-After on 429")
	}
	assertProblem(t, rr)
}

func TestHealthzAndHelloStillWork(t *testing.T) {
	g := NewGateway(Config{})
	if rr := do(t, g, http.MethodGet, "/healthz", "", nil); rr.Code != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200", rr.Code)
	}
	if rr := do(t, g, http.MethodGet, "/api/v1/hello", "", nil); rr.Code != http.StatusOK {
		t.Fatalf("hello status = %d, want 200", rr.Code)
	}
}

func mustJSON(t *testing.T, rr *httptest.ResponseRecorder, dest any) {
	t.Helper()
	if err := json.Unmarshal(rr.Body.Bytes(), dest); err != nil {
		t.Fatalf("decode body %q: %v", rr.Body, err)
	}
}

func assertProblem(t *testing.T, rr *httptest.ResponseRecorder) {
	t.Helper()
	if ct := rr.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("Content-Type = %q, want application/problem+json", ct)
	}
	var p Problem
	mustJSON(t, rr, &p)
	if p.Status != rr.Code {
		t.Fatalf("problem.status = %d, want %d", p.Status, rr.Code)
	}
	if p.Title == "" || p.Type == "" {
		t.Fatalf("problem missing title/type: %+v", p)
	}
}
