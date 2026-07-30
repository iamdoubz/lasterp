# WP-1.4b — Boot assembly + action surface: decisions

Interpretations made where the roadmap entry (docs/11, WP-1.4b) and the
phase-1 midpoint review (docs/notes/phase-1-midpoint.md, Finding 1) leave room.
Nothing here relitigates an ADR.

## 1. Composition root lives in `internal/app`, not `cmd/lasterp`

The WP wires modules (`modules/*`) into the kernel gateway. Kernel must not
import modules (CLAUDE.md), so the wiring can't live in `kernel/api`. It goes in
a new top-level **composition root** `internal/app`, which may import both
`kernel/*` and `modules/*` — it is neither kernel nor a module, it is the
product assembly. `cmd/lasterp` shrinks to flag/env parsing + signal handling
and calls `app.Open` / `app.Handler`. The boot e2e test lives in `internal/app`
so it can seed fixtures in-process and drive the real handler over HTTP.

## 2. Objects exposed as generic CRUD vs. bespoke action routes

`gateway.registerObject` wires all five REST verbs for an object. For
financial-control objects that is a hole, so the composition root is selective:

- **Generic CRUD (`Config.Objects`):** `Account`, `Contact`. Arbitrary field
  edits on these are safe.
- **Bespoke routes (NOT in `Config.Objects`):**
  - `Invoice` — a generic `POST`/`PATCH` would let a client set
    `status=posted` with hand-picked totals and no GL entry or number,
    bypassing the posting pipeline (**INV-F5/F6**) and posted-doc immutability
    (**INV-F2**). Exposed via `CreateDraft`/`UpdateDraft`/`GetInvoice`/PDF/post
    actions instead.
  - `Period` — a generic `PATCH status` would bypass monotonic close and the
    privileged/audited reopen (**INV-F3**). Exposed via create + get +
    close/reopen actions; **no generic update/delete**.
  - `JournalEntry` — event-sourced, has no CRUD table (`registerObject` already
    skips it). Exposed via a read route + reverse action.

## 3. Action route mechanism

`kernel/api.Config` grows `Actions []Action`. An `Action` is
`{Method, Path, Object, Summary, Write, Handler}`. The gateway wraps each with
the **same** choke point as CRUD: `guard` (authn → tenant-mismatch guard →
rate-limit → actor bind) then `capabilityGate(Object)`; write actions
additionally go through `handleWrite` (idempotency-key required — the "all
writes take idempotency keys" hard rule). The domain logic in each `Handler`
lives in `internal/app` (which may import modules); `kernel/api` supplies only
the plumbing and the OpenAPI doc entry. No layering violation.

Action surface built (the WP's parenthetical list):

| Method | Path | Calls |
|---|---|---|
| POST | `/api/v1/invoices` | `invoicing.CreateDraft` |
| PATCH | `/api/v1/invoices/{id}` | `invoicing.UpdateDraft` |
| GET | `/api/v1/invoices/{id}` | `invoicing.GetInvoice` |
| GET | `/api/v1/invoices/{id}/pdf` | `invoicing.RenderInvoicePDF` |
| POST | `/api/v1/invoices/{id}/post` | `invoicing.PostInvoice` |
| POST | `/api/v1/periods` | `ledger.CreatePeriod` |
| GET | `/api/v1/periods/{id}` | CRUD get |
| POST | `/api/v1/periods/{id}/close` | `ledger.ClosePeriod` |
| POST | `/api/v1/periods/{id}/reopen` | `ledger.ReopenPeriod` |
| GET | `/api/v1/journalentries/{id}` | `ledger.LoadEntry` |
| POST | `/api/v1/journalentries/{id}/reverse` | `ledger.Reverse` |
| POST | `/api/v1/taxrates` | `tax.SaveRate` (authorized — §4) |
| POST | `/api/v1/fxrates` | `money.SaveRate` (authorized — §4) |
| GET | `/api/v1/capabilities` | `capability.EnabledModules` |
| POST | `/api/v1/capabilities/{module}/enable` | `capability.Enable` |
| POST | `/api/v1/capabilities/{module}/disable` | `capability.Disable` |

Accounts are created via the generic `Account` CRUD `POST` (no bespoke route).

## 4. The tax/FX reference-data authz seam lands at the API boundary

WP-1.1/1.3 deferred authz on `tax.SaveRate` / `money.SaveRate` as "safe while no
API surface exists." The surface now exists, so the seam lands: the **API
handlers** call `authz.Authorize(ctx, db, "TaxRate"/"FxRate", "manage")` before
delegating. The domain `SaveRate` functions stay authz-free because they are
also the **boot seeding path** (`tax.LoadSeedPacks` writes global pack rows at
startup with no actor) — a system path, exactly like running migrations. The
only *tenant-reachable* write path to these tables is the API, and it
authorizes (**INV-T2**); RLS still enforces isolation (**INV-T1**). New
permission tuples: `TaxRate:manage`, `FxRate:manage`. AC 4 ("unauthorized
tax-rate write via API rejected") is a principal lacking `TaxRate:manage` → 403.

## 5. Authentication: bearer session token → `identity.ValidateSession`

The gateway `Authenticator` extracts a `Bearer` token from `Authorization` and
resolves it via `identity.ValidateSession` to `(authz.Actor{TenantID,UserID},
tenant)`. `actor.TenantID == tenant` by construction, so the gateway's
tenant-mismatch guard passes. **No HTTP login/session-issuance route is built
here** — issuing sessions over HTTP (password/TOTP login UX, OIDC) is WP-1.5 /
WP-1.9. The e2e test issues a session in-process (`identity.IssueSession`) and
drives HTTP with the bearer token; that is sufficient to prove authn is
enforced on every route. Flagged as deferred.

## 6. All modules are `Register()`ed at boot; capability state gates per tenant

`Register()` creates global schema (tables, triggers); capability enable-state
is per-tenant. So boot registers **all** built-in modules unconditionally
(contacts, ledger, tax, invoicing) to create their tables, and the
`capability.GatewayChecker` gates access per tenant at request time. "module
`Register()` per capability state" is read as "register the modules in the
build," not "conditionally create tables."

## 7. DB role separation (the REVOKEs) is a deploy step, not a boot step

Migrations create the `append_event`/`ledger_post_entry` SECURITY DEFINER
functions, so the posting happy-path works at boot regardless of grants. The
**enforcement** side — `EnforceAppendOnlyGrants`/`EnforceLedgerPipelineGrants`
(REVOKE INSERT on `events` from the app role) — requires a provisioned app role
distinct from the migration/owner role, which is a deployment-topology concern
(WP-10.x) and is already proven under real posture by the module integrity
tests. Boot runs migrations only. The boot e2e test, however, **does** stand up
the locked-down app-role posture (mirroring `modules/ledger/testdb_test.go`) so
the full HTTP lifecycle is exercised with the event log locked down — proving
boot assembly composes correctly with role separation, not just in a superuser
sandbox.

## 8. `LASTERP_DSN` selects the dialect

`cmd/lasterp` reads `LASTERP_DSN` (and `LASTERP_ADDR`, default `:8080`). A DSN
beginning `postgres://` or `postgresql://` → Postgres adapter; anything else (or
empty) → SQLite at that path (default `lasterp.db`). Covers AC's "fresh Postgres
AND SQLite."

## 9. No new INV-* catalog entry

This WP adds no new invariant; it exposes existing ones over HTTP and lands the
deferred **INV-T2** authz seam for reference data. The boot e2e is tagged
`//go:build integrity` and references the invariants it exercises
(INV-T1/T2/F2/F5/F6). Registry gate is unchanged.

## Deferred (flagged)

- HTTP login / session issuance (password/TOTP, OIDC) → WP-1.5 / WP-1.9.
- Tenant/user/role bootstrap CLI (`lasterp init`) → deploy tooling.
- App-role provisioning + auto-applying role-separation grants at boot →
  WP-10.x deployment.
- Tax/FX rate *list/read* API and richer admin (only writes are in the WP's
  parenthetical) — read-back is via the domain funcs for now.
- Metadata-declared action routes (ADR-009 "metadata-declared first"): actions
  are hand-registered structs in v1; a metadata `actions:` block is a later WP.
