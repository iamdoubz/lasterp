// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/iamdoubz/lasterp/kernel/authz"
	"github.com/iamdoubz/lasterp/kernel/metadata"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// ErrAuthUnavailable is how an Authenticator says "I could not decide",
// as distinct from "this caller is not authenticated".
//
// The distinction is the whole point: a 401 tells a client its credential is
// no good, and clients act on that — they sign the user out, and an offline
// outbox files its queued work as refused. A transient failure to *reach* the
// session store must not say that, so it becomes a 503 with Retry-After.
// Authenticators wrap infrastructure errors with this; a genuinely invalid
// credential is returned bare.
var ErrAuthUnavailable = errors.New("api: authentication backend unavailable")

// ErrDeviceWiped is how an Authenticator says "this credential is good and the
// device it belongs to has been remotely wiped" (WP-2.5, INV-D1).
//
// Same shape of distinction as ErrAuthUnavailable above and the opposite
// intent: that one exists so a client does *not* act destructively on a
// transient failure, this one exists so a client *does* act destructively on a
// deliberate instruction. A bare 401 would have the device sign the user out
// and keep its replica, which is precisely what a wipe must prevent.
var ErrDeviceWiped = errors.New("api: device has been wiped")

// Authenticator resolves a request to the authenticated actor and tenant.
// It is the gateway's authn seam: token/session verification (OAuth 2.1,
// PATs — WP-3.7, docs/15) is not built in this WP, so the concrete
// implementation is injected. Returning an error fails the request closed
// (401, or 503 for ErrAuthUnavailable). See WP-0.6-decisions.md, decision 5.
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

// SchemaResolver returns an object's schema as one tenant sees it: core plus
// that tenant's stored overlays (ADR-006, WP-3.2c). It is the gateway's
// customization seam — kernel/api owns the choke point, kernel/metadata owns
// the overlay store — satisfied structurally by metadata.DBResolver.
//
// Nil means "no tenant has customized anything", which is the behaviour of
// every deployment before WP-3.2c and of every test that wires a gateway by
// hand: routes then serve the boot schema, exactly as they always did.
type SchemaResolver interface {
	Resolve(ctx context.Context, tenant tenancy.ID, core *metadata.Object) (*metadata.EffectiveSchema, error)
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

	// Public exempts the route from authentication — the narrow escape hatch
	// that lets session issuance exist at all: a login route cannot present a
	// session it has not yet issued (WP-1.5-decisions.md §4). It skips *only*
	// authn; rate limiting (keyed by client IP, since there is no actor yet),
	// problem+json errors, and OpenAPI documentation all still apply, and the
	// handler receives an empty tenant.
	//
	// Public is an INV-T2 hole by construction ("no write path executes without
	// an authenticated principal"), so it is deliberately constrained: a public
	// action must be a read (Write false) and capability-ungated (Object ""),
	// enforced at boot by NewGateway and in CI by the route-enumeration test in
	// internal/app. Do not widen it — add an authenticated route instead.
	Public bool

	// CarriesCredentials marks a write whose request or response body holds a
	// secret — a password, a recovery code, an API key. It keeps both out of
	// idempotency_keys, a table with no TTL and no cleanup, which would
	// otherwise become the longest-lived copy of every credential the API
	// touches (WP-1.12-decisions.md §9).
	//
	// Two effects, one per column:
	//
	//   - response_body is stored empty, so a replay returns the original
	//     status and Idempotent-Replayed and nothing else. That is the right
	//     semantics for a show-once credential anyway: replaying a confirmation
	//     must not re-reveal it.
	//   - request_fingerprint is computed over method and path only, never the
	//     body. An unsalted SHA-256 of {"password":"…"} is an offline oracle
	//     against a value that is deliberately expensive to guess everywhere
	//     else — bcrypt in users.password_hash is pointless if the same secret
	//     sits one table over behind a single hash.
	//
	// The cost is that two different bodies under one key read as a replay
	// rather than a 409 on these routes. That is acceptable where it is set:
	// the request is the credential, so a caller reusing a key with a different
	// body is retrying, not issuing a new command.
	//
	// It is not a way to opt out of idempotency. The reservation and the
	// exactly-once execution are unchanged.
	CarriesCredentials bool
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
	// Hooks, when set, dispatches synchronous plugin hooks inside every CRUD
	// write (WP-3.1b). Nil ⇒ no dispatch, which is the behaviour of any
	// deployment with no plugin host wired.
	Hooks metadata.Hooks
	// Schemas, when set, resolves each request's object schema against the
	// calling tenant's overlays (WP-3.2c). Nil ⇒ every tenant gets the boot
	// schema, which is what every deployment before overlays existed got.
	Schemas SchemaResolver
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
	hooks   metadata.Hooks
	schemas SchemaResolver
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
		hooks:   cfg.Hooks,
		schemas: cfg.Schemas,
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
//
// The *routes* are registered once, from the boot schema list, because an
// overlay adds fields to a shipped object and never a new object — a fully
// custom object needs per-tenant DDL and is not built here
// (WP-3.2c-decisions.md §2). The *engine* behind them is per request, so the
// fields those routes accept and return are the calling tenant's.
func (g *Gateway) registerObject(schema *metadata.EffectiveSchema) {
	engine, err := g.engineFor(schema)
	if err != nil {
		// Non-CRUD (event-sourced) objects have no REST CRUD surface yet;
		// skip rather than fail the whole gateway.
		return
	}
	base := "/api/v1/" + resourcePath(schema.ObjectName)
	object := schema.ObjectName
	gate := func(h apiHandler) http.HandlerFunc { return g.guard(g.capabilityGate(object, h)) }

	g.mux.HandleFunc("GET "+base, gate(g.handleList(engine)))
	g.mux.HandleFunc("POST "+base, gate(g.handleWrite(g.doCreate(engine), false)))
	g.mux.HandleFunc("GET "+base+"/{id}", gate(g.handleGet(engine)))
	g.mux.HandleFunc("PATCH "+base+"/{id}", gate(g.handleWrite(g.doUpdate(engine), false)))
	g.mux.HandleFunc("DELETE "+base+"/{id}", gate(g.handleWrite(g.doDelete(engine), false)))
}

// crudSource hands a handler the CRUD engine for its object as the calling
// tenant sees it.
type crudSource func(r *http.Request, tenant tenancy.ID) (*metadata.CRUD, error)

// engineFor builds one object's crudSource, or errors for an object with no
// CRUD surface at all. With no resolver configured it closes over a single
// engine built at boot; with one, it resolves per request.
//
// Resolving per request is cheap on purpose: NewCRUD is a struct wrap, so the
// only cost is the resolver's read, which is one indexed SELECT against a table
// that is empty for a tenant that has customized nothing (see metadata.Resolve
// for why there is no cache in front of it).
func (g *Gateway) engineFor(schema *metadata.EffectiveSchema) (crudSource, error) {
	withHooks := func(c *metadata.CRUD) *metadata.CRUD {
		if g.hooks != nil {
			return c.WithHooks(g.hooks)
		}
		return c
	}
	boot, err := metadata.NewCRUD(schema)
	if err != nil {
		return nil, err
	}
	if g.schemas == nil {
		boot = withHooks(boot)
		return func(*http.Request, tenancy.ID) (*metadata.CRUD, error) { return boot, nil }, nil
	}
	// The *core* object, not the boot effective schema: merging overlays onto
	// an already-merged schema would stack a layer twice.
	core := schema.Object
	return func(r *http.Request, tenant tenancy.ID) (*metadata.CRUD, error) {
		eff, err := g.schemas.Resolve(r.Context(), tenant, &core)
		if err != nil {
			return nil, err
		}
		crud, err := metadata.NewCRUD(eff)
		if err != nil {
			return nil, err
		}
		return withHooks(crud), nil
	}, nil
}

// registerAction wires one non-CRUD Action onto the mux through the same
// choke point as CRUD routes: guard (authn → tenant-mismatch → rate limit →
// actor bind) then the capability gate; write actions add idempotency.
//
// Public actions take the reduced path (rate limit only). The constraints on
// Public are checked here and panic on violation: an unauthenticated write or
// a capability-gated route with no tenant to gate on is a static
// misconfiguration that must never reach a running server (INV-T2).
func (g *Gateway) registerAction(a Action) {
	if a.Public {
		if a.Write {
			panic("api: public action " + a.Method + " " + a.Path + " must not be a write (INV-T2)")
		}
		if a.Object != "" {
			panic("api: public action " + a.Method + " " + a.Path + " cannot be capability-gated (no tenant before authn)")
		}
		g.mux.HandleFunc(a.Method+" "+a.Path, g.guardPublic(a.Handler))
		return
	}
	var h apiHandler = a.Handler
	if a.Write {
		h = g.handleWrite(writeExec(a.Handler), a.CarriesCredentials)
	}
	handler := g.guard(g.capabilityGate(a.Object, h))
	g.mux.HandleFunc(a.Method+" "+a.Path, handler)
}

// guardPublic is guard minus authentication: it rate-limits by client IP and
// hands the handler an empty tenant. Login and refresh are the only routes that
// may use it — they establish the very session guard would demand.
func (g *Gateway) guardPublic(h apiHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		d := g.limiter.allow("ip\x00" + clientIP(r))
		setRateLimitHeaders(w, d)
		if d.limited {
			w.Header().Set("Retry-After", strconv.Itoa(d.resetSecs))
			writeProblem(w, Problem{Status: http.StatusTooManyRequests, Title: "rate limit exceeded", Instance: r.URL.Path})
			return
		}
		h(w, r, "")
	}
}

// clientIP is the rate-limit key for unauthenticated routes. It reads
// r.RemoteAddr only: X-Forwarded-For is caller-supplied and would let an
// attacker mint a fresh login budget per forged header. A deployment behind a
// trusted proxy should have the proxy rewrite RemoteAddr (or terminate the
// limit itself) rather than have the gateway trust a header it cannot verify.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
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
			// "Could not check" is not "not authenticated". Every Authenticator
			// error used to land here as a 401, which meant a database too busy
			// to answer the session lookup was reported as a bad credential —
			// and a 401 is the one status this product's clients act on
			// destructively: the web client signs the user out on it
			// (api/client.ts isUnauthenticated), and the sync drain would file
			// queued offline work as rejected rather than retrying it
			// (INV-S4). Found by the WP-2.3b simulation harness, whose
			// concurrent writers put the SQLite session lookup under contention
			// for the first time.
			if errors.Is(err, ErrAuthUnavailable) {
				w.Header().Set("Retry-After", "1")
				writeProblem(w, Problem{Status: http.StatusServiceUnavailable, Title: "authentication temporarily unavailable", Instance: r.URL.Path})
				return
			}
			// The one 401 that carries a reason. Everything below this line is
			// deliberately opaque so a caller cannot probe which half of a
			// credential was wrong; a wipe is the documented exception, because
			// the instruction is useless unless the device can recognise it
			// (INV-D1, WP-2.5-decisions.md §3).
			if errors.Is(err, ErrDeviceWiped) {
				writeProblem(w, Problem{
					Type:     ProblemDeviceWiped,
					Status:   http.StatusUnauthorized,
					Title:    "device has been wiped",
					Detail:   "this device was remotely wiped by an administrator; its local data must be destroyed",
					Instance: r.URL.Path,
				})
				return
			}
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

func (g *Gateway) handleList(src crudSource) apiHandler {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		crud, err := src(r, tenant)
		if err != nil {
			writeProblem(w, problemForError(err, r.URL.Path))
			return
		}
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

func (g *Gateway) handleGet(src crudSource) apiHandler {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		crud, err := src(r, tenant)
		if err != nil {
			writeProblem(w, problemForError(err, r.URL.Path))
			return
		}
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

func (g *Gateway) doCreate(src crudSource) writeExec {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		crud, err := src(r, tenant)
		if err != nil {
			writeProblem(w, problemForError(err, r.URL.Path))
			return
		}
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

func (g *Gateway) doUpdate(src crudSource) writeExec {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		crud, err := src(r, tenant)
		if err != nil {
			writeProblem(w, problemForError(err, r.URL.Path))
			return
		}
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

func (g *Gateway) doDelete(src crudSource) writeExec {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		crud, err := src(r, tenant)
		if err != nil {
			writeProblem(w, problemForError(err, r.URL.Path))
			return
		}
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
func (g *Gateway) handleWrite(exec writeExec, credentialed bool) apiHandler {
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
		// A credentialed route fingerprints method and path only: the body is
		// the secret, and a stored hash of it outlives every other copy.
		fpBody := body
		if credentialed {
			fpBody = nil
		}
		fp := fingerprint(r.Method, r.URL.Path, fpBody)

		stored, err := g.idem.begin(r.Context(), tenant, key, fp)
		switch {
		case err == nil: // replay
			writeStored(w, stored)
			return
		case errors.Is(err, errKeyConflict):
			// Typed, because a client cannot act on it otherwise. This 409
			// covers a key reused with a different fingerprint *and* a key
			// whose original request is still in flight — and the second is
			// the ordinary consequence of a client crashing mid-write and
			// re-sending. An offline outbox that read it as a rejection would
			// file a conflict for a command that is at that moment succeeding
			// (WP-2.3-decisions.md §11), so the drain has to be able to tell
			// this apart from "the server refused your work" and retry.
			writeProblem(w, Problem{Type: ProblemIdempotencyConflict, Status: http.StatusConflict, Title: "idempotency key conflict", Detail: "the Idempotency-Key was already used for a different or in-flight request", Instance: r.URL.Path})
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
			stored := cw.buf.Bytes()
			if credentialed {
				stored = nil
			}
			if ferr := g.idem.finalize(r.Context(), tenant, key, cw.status, stored); ferr != nil {
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
