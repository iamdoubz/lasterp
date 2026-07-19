// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/iamdoubz/lasterp/kernel/authz"
	"github.com/iamdoubz/lasterp/kernel/metadata"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// Authenticator resolves a request to the authenticated actor and tenant.
// It is the gateway's authn seam: token/session verification (OAuth 2.1,
// PATs — WP-3.7, docs/15) is not built in this WP, so the concrete
// implementation is injected. Returning an error fails the request closed
// (401). See WP-0.6-decisions.md, decision 5.
type Authenticator interface {
	Authenticate(r *http.Request) (authz.Actor, tenancy.ID, error)
}

// AuthenticatorFunc adapts a function to Authenticator.
type AuthenticatorFunc func(r *http.Request) (authz.Actor, tenancy.ID, error)

// Authenticate implements Authenticator.
func (f AuthenticatorFunc) Authenticate(r *http.Request) (authz.Actor, tenancy.ID, error) {
	return f(r)
}

// CapabilityChecker reports whether the module owning object is enabled for
// tenant (ADR-018 §5). It is the gateway's composability seam: when set, a
// request to an object whose module is disabled gets a capability-disabled
// problem+json instead of a confusing 403/404. An object owned by no module
// (a kernel object) reports enabled=true. Satisfied structurally by
// capability.GatewayChecker; kept as an interface here so kernel/api does not
// import kernel/capability.
type CapabilityChecker interface {
	Enabled(ctx context.Context, tenant tenancy.ID, object string) (enabled bool, capability string, err error)
}

// Action is a non-CRUD gateway route: a lifecycle verb (post invoice, reverse
// entry, close/reopen period), a read of an event-sourced object, or a
// reference-data / capability admin write. The composition root
// (internal/app) builds these from module funcs — kernel/api must not import
// modules — while the gateway supplies the shared choke point (authn,
// tenant-mismatch guard, rate limit, capability gate, idempotency) and the
// OpenAPI documentation, exactly as it does for CRUD routes.
//
// Handler runs after authn: the actor is bound into r.Context() and tenant is
// passed explicitly (== actor.TenantID). Write actions are additionally wrapped
// with idempotency (an Idempotency-Key is required — the "all writes take
// idempotency keys" rule), so their Handler must be safe to run against a
// capture buffer.
type Action struct {
	Method  string // "GET", "POST", "PATCH"
	Path    string // full route pattern, e.g. "/api/v1/invoices/{id}/post"
	Object  string // metadata object for capability gating; "" ⇒ ungated
	Summary string // OpenAPI summary
	Write   bool   // wrap with idempotency (writes) vs. plain read
	Handler HandlerFunc
}

// HandlerFunc is an Action handler: it runs after the gateway choke point has
// authenticated the request (the actor is bound into r.Context()) and passes
// the resolved tenant (== actor.TenantID) explicitly. It matches apiHandler,
// the internal CRUD handler shape.
type HandlerFunc = func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID)

// Config configures a Gateway. All fields are optional: with the zero value
// the gateway still serves /healthz, /api/v1/hello and an (object-less)
// OpenAPI document. CRUD routes exist only for registered Objects and
// require both a DB and an Authenticator.
type Config struct {
	DB            *storage.DB
	Objects       []*metadata.EffectiveSchema
	Actions       []Action
	Authenticator Authenticator
	RateLimit     RateLimit
	// Capabilities, when set, gates object routes behind their module's
	// enable-state (ADR-018 §5). Nil ⇒ every object is always reachable.
	Capabilities CapabilityChecker
	// Now overrides the clock (rate limiter, timestamps) for tests.
	Now func() time.Time
}

// Gateway is the metadata-driven REST API surface (WP-0.6, ADR-009).
type Gateway struct {
	mux     *http.ServeMux
	db      *storage.DB
	auth    Authenticator
	caps    CapabilityChecker
	idem    *idempotencyStore
	limiter *rateLimiter
	objects []*metadata.EffectiveSchema
	actions []Action
}

// defaultRateLimit is applied when Config.RateLimit is the zero value: a
// generous per-caller budget so ordinary use is never throttled while still
// exercising the limiter path.
var defaultRateLimit = RateLimit{RequestsPerSecond: 100, Burst: 200}

// NewGateway builds the gateway handler from cfg.
func NewGateway(cfg Config) *Gateway {
	now := cfg.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	rl := cfg.RateLimit
	if rl.RequestsPerSecond == 0 && rl.Burst == 0 {
		rl = defaultRateLimit
	}

	g := &Gateway{
		mux:     http.NewServeMux(),
		db:      cfg.DB,
		auth:    cfg.Authenticator,
		caps:    cfg.Capabilities,
		limiter: newRateLimiter(rl, now),
		objects: cfg.Objects,
		actions: cfg.Actions,
	}
	if cfg.DB != nil {
		g.idem = &idempotencyStore{db: cfg.DB, now: now}
	}

	g.mux.HandleFunc("GET /healthz", handleHealthz)
	g.mux.HandleFunc("GET /api/v1/hello", handleHello)
	g.mux.HandleFunc("GET /api/v1/openapi.json", g.handleOpenAPI)
	// Catch-all: anything unmatched is a problem+json 404, not net/http's
	// plain-text default.
	g.mux.HandleFunc("/", g.handleNotFound)

	for _, schema := range g.objects {
		g.registerObject(schema)
	}
	for _, a := range g.actions {
		g.registerAction(a)
	}
	return g
}

// NewMux returns the kernel API handler with the bootstrap routes only
// (health + hello + object-less OpenAPI). Kept for cmd/lasterp and the
// bootstrap tests; richer deployments call NewGateway with a DB + objects.
func NewMux() http.Handler { return NewGateway(Config{}) }

// ServeHTTP implements http.Handler.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) { g.mux.ServeHTTP(w, r) }

func (g *Gateway) handleOpenAPI(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, OpenAPI(g.objects, g.actions))
}

func (g *Gateway) handleNotFound(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, Problem{Status: http.StatusNotFound, Title: "not found", Instance: r.URL.Path})
}

// registerObject wires the five REST routes for one object onto the mux.
func (g *Gateway) registerObject(schema *metadata.EffectiveSchema) {
	crud, err := metadata.NewCRUD(schema)
	if err != nil {
		// Non-CRUD (event-sourced) objects have no REST CRUD surface yet;
		// skip rather than fail the whole gateway.
		return
	}
	base := "/api/v1/" + resourcePath(schema.ObjectName)
	object := schema.ObjectName
	gate := func(h apiHandler) http.HandlerFunc { return g.guard(g.capabilityGate(object, h)) }

	g.mux.HandleFunc("GET "+base, gate(g.handleList(crud)))
	g.mux.HandleFunc("POST "+base, gate(g.handleWrite(g.doCreate(crud))))
	g.mux.HandleFunc("GET "+base+"/{id}", gate(g.handleGet(crud)))
	g.mux.HandleFunc("PATCH "+base+"/{id}", gate(g.handleWrite(g.doUpdate(crud))))
	g.mux.HandleFunc("DELETE "+base+"/{id}", gate(g.handleWrite(g.doDelete(crud))))
}

// registerAction wires one non-CRUD Action onto the mux through the same
// choke point as CRUD routes: guard (authn → tenant-mismatch → rate limit →
// actor bind) then the capability gate; write actions add idempotency.
func (g *Gateway) registerAction(a Action) {
	var h apiHandler = a.Handler
	if a.Write {
		h = g.handleWrite(writeExec(a.Handler))
	}
	handler := g.guard(g.capabilityGate(a.Object, h))
	g.mux.HandleFunc(a.Method+" "+a.Path, handler)
}

// capabilityGate rejects a request for object with a capability-disabled
// problem+json when the object's module is disabled for the tenant (ADR-018
// §5). No checker configured ⇒ pass through.
func (g *Gateway) capabilityGate(object string, h apiHandler) apiHandler {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		if g.caps != nil {
			enabled, capName, err := g.caps.Enabled(r.Context(), tenant, object)
			if err != nil {
				writeProblem(w, Problem{Status: http.StatusInternalServerError, Title: "internal server error", Instance: r.URL.Path})
				return
			}
			if !enabled {
				writeProblem(w, Problem{
					Type:     "capability-disabled",
					Status:   http.StatusForbidden,
					Title:    "capability disabled",
					Detail:   "the " + capName + " capability is not enabled for this tenant",
					Instance: r.URL.Path,
				})
				return
			}
		}
		h(w, r, tenant)
	}
}

// apiHandler is a CRUD handler that has already been authenticated and
// rate-limited; the actor is bound into r.Context() and tenant is passed
// explicitly.
type apiHandler func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID)

// guard authenticates, rate-limits, binds the actor to the request context,
// then delegates. It is the single gateway choke point (ADR-009: "single
// gateway enforces authn, tenant context, rate limits").
func (g *Gateway) guard(h apiHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if g.auth == nil {
			writeProblem(w, Problem{Status: http.StatusUnauthorized, Title: "authentication required", Instance: r.URL.Path})
			return
		}
		actor, tenant, err := g.auth.Authenticate(r)
		if err != nil {
			// Do not echo the Authenticator's error into the body: it can carry
			// token/session internals (info leak, phase-0-review WP-0.6 nit).
			// The reason belongs in server logs, not the 401 to the caller.
			writeProblem(w, Problem{Status: http.StatusUnauthorized, Title: "authentication required", Instance: r.URL.Path})
			return
		}
		// Single source of truth for the tenant a write lands in: authz
		// filters on actor.TenantID, so the CRUD call must use the same value
		// (below). A divergent (actor, tenant) pair from a buggy/hostile
		// Authenticator would otherwise authorize against one tenant and write
		// to another (INV-T1/INV-T2 hole) — reject it outright, fail closed.
		if actor.TenantID != tenant {
			writeProblem(w, Problem{Status: http.StatusForbidden, Title: "tenant mismatch", Instance: r.URL.Path})
			return
		}

		d := g.limiter.allow(rateKey(actor.TenantID, actor))
		setRateLimitHeaders(w, d)
		if d.limited {
			w.Header().Set("Retry-After", strconv.Itoa(d.resetSecs))
			writeProblem(w, Problem{Status: http.StatusTooManyRequests, Title: "rate limit exceeded", Instance: r.URL.Path})
			return
		}

		ctx := authz.WithActor(r.Context(), actor)
		h(w, r.WithContext(ctx), actor.TenantID)
	}
}

func rateKey(tenant tenancy.ID, actor authz.Actor) string {
	return string(tenant) + "\x00" + string(actor.UserID)
}

func setRateLimitHeaders(w http.ResponseWriter, d decision) {
	if d.limit == 0 {
		return // limiting disabled
	}
	w.Header().Set("RateLimit-Limit", strconv.Itoa(d.limit))
	w.Header().Set("RateLimit-Remaining", strconv.Itoa(d.remaining))
	w.Header().Set("RateLimit-Reset", strconv.Itoa(d.resetSecs))
}

// --- read handlers ---

func (g *Gateway) handleList(crud *metadata.CRUD) apiHandler {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		records, err := crud.List(r.Context(), g.db, tenant)
		if err != nil {
			writeProblem(w, problemForError(err, r.URL.Path))
			return
		}
		if records == nil {
			records = []metadata.Record{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": records})
	}
}

func (g *Gateway) handleGet(crud *metadata.CRUD) apiHandler {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		rec, err := crud.Get(r.Context(), g.db, tenant, r.PathValue("id"))
		if err != nil {
			writeProblem(w, problemForError(err, r.URL.Path))
			return
		}
		writeJSON(w, http.StatusOK, rec)
	}
}

// --- write handlers (wrapped with idempotency) ---

// writeExec runs one CRUD mutation, writing its response to w (a capture
// buffer). It returns nothing; the HTTP status it writes drives whether the
// idempotency reservation is finalized (2xx) or discarded.
type writeExec func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID)

func (g *Gateway) doCreate(crud *metadata.CRUD) writeExec {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		rec, ok := decodeRecord(w, r)
		if !ok {
			return
		}
		created, err := crud.Create(r.Context(), g.db, tenant, rec)
		if err != nil {
			writeProblem(w, problemForError(err, r.URL.Path))
			return
		}
		writeJSON(w, http.StatusCreated, created)
	}
}

func (g *Gateway) doUpdate(crud *metadata.CRUD) writeExec {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		rec, ok := decodeRecord(w, r)
		if !ok {
			return
		}
		updated, err := crud.Update(r.Context(), g.db, tenant, r.PathValue("id"), rec)
		if err != nil {
			writeProblem(w, problemForError(err, r.URL.Path))
			return
		}
		writeJSON(w, http.StatusOK, updated)
	}
}

func (g *Gateway) doDelete(crud *metadata.CRUD) writeExec {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		if err := crud.SoftDelete(r.Context(), g.db, tenant, r.PathValue("id")); err != nil {
			writeProblem(w, problemForError(err, r.URL.Path))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// handleWrite wraps a mutation with idempotency: it requires an
// Idempotency-Key, replays a stored response on a matching key, otherwise
// executes exec once and records the result (ADR-009).
func (g *Gateway) handleWrite(exec writeExec) apiHandler {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		key := r.Header.Get("Idempotency-Key")
		if key == "" {
			writeProblem(w, Problem{Status: http.StatusBadRequest, Title: "missing Idempotency-Key header", Detail: "all writes require an Idempotency-Key header (ADR-009)", Instance: r.URL.Path})
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeProblem(w, Problem{Status: http.StatusBadRequest, Title: "unreadable request body", Instance: r.URL.Path})
			return
		}
		fp := fingerprint(r.Method, r.URL.Path, body)

		stored, err := g.idem.begin(r.Context(), tenant, key, fp)
		switch {
		case err == nil: // replay
			writeStored(w, stored)
			return
		case errors.Is(err, errKeyConflict):
			writeProblem(w, Problem{Status: http.StatusConflict, Title: "idempotency key conflict", Detail: "the Idempotency-Key was already used for a different or in-flight request", Instance: r.URL.Path})
			return
		case !errors.Is(err, errReserved):
			writeProblem(w, problemForError(err, r.URL.Path))
			return
		}

		// Reserved: execute once against a capture buffer.
		r.Body = io.NopCloser(bytes.NewReader(body))
		cw := &captureWriter{header: make(http.Header)}
		exec(cw, r, tenant)

		if cw.status >= 200 && cw.status < 300 {
			if ferr := g.idem.finalize(r.Context(), tenant, key, cw.status, cw.buf.Bytes()); ferr != nil {
				writeProblem(w, problemForError(ferr, r.URL.Path))
				return
			}
		} else if derr := g.idem.discard(r.Context(), tenant, key); derr != nil {
			writeProblem(w, problemForError(derr, r.URL.Path))
			return
		}
		cw.flush(w)
	}
}

func writeStored(w http.ResponseWriter, s *storedResponse) {
	if len(s.body) > 0 {
		w.Header().Set("Content-Type", "application/json")
	}
	w.Header().Set("Idempotent-Replayed", "true")
	w.WriteHeader(s.status)
	_, _ = w.Write(s.body)
}

// decodeRecord parses a JSON object request body into a metadata.Record,
// writing a 400 problem and reporting false on malformed input.
func decodeRecord(w http.ResponseWriter, r *http.Request) (metadata.Record, bool) {
	var rec metadata.Record
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rec); err != nil {
		writeProblem(w, Problem{Status: http.StatusBadRequest, Title: "malformed JSON body", Detail: err.Error(), Instance: r.URL.Path})
		return nil, false
	}
	return rec, true
}

// captureWriter buffers a handler's response so handleWrite can persist it
// under the idempotency key before flushing it to the real ResponseWriter.
type captureWriter struct {
	header http.Header
	status int
	buf    bytes.Buffer
}

func (c *captureWriter) Header() http.Header { return c.header }

func (c *captureWriter) WriteHeader(status int) {
	if c.status == 0 {
		c.status = status
	}
}

func (c *captureWriter) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	return c.buf.Write(b)
}

func (c *captureWriter) flush(w http.ResponseWriter) {
	for k, vs := range c.header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	if c.status == 0 {
		c.status = http.StatusOK
	}
	w.WriteHeader(c.status)
	_, _ = w.Write(c.buf.Bytes())
}
