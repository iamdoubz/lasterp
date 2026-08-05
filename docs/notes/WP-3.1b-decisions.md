# WP-3.1b — The hook surface: decisions

PR-B of WP-3.1. The scope and the two shaping calls were settled by the plan review recorded in
[WP-3.1-decisions.md](WP-3.1-decisions.md) §8 and reflected in [docs/05](../05-PLUGIN-SYSTEM.md)
and the roadmap; this file resolves what building it exposes. Written before the code.

## 1. The dispatch seam is an interface in `kernel/metadata`, implemented by `kernel/plugins`

`kernel/plugins` imports `kernel/metadata` (it needs `CRUD` to serve `object.*` host calls), so
metadata cannot import plugins back — that is an import cycle, and CLAUDE.md forbids them outright.

The dispatch point still has to be **inside `CRUD`**, not around it. Wrapping CRUD at the
composition root would cover the HTTP routes and the sync drain (both go through the ordinary
routes) but not a module calling `CRUD.Create` directly — a hook surface with a bypass is not one.

So `kernel/metadata` declares the small interface it consumes:

```go
// in kernel/metadata
type Hooks interface {
    Before(ctx context.Context, tenant tenancy.ID, object, verb string, rec Record) (Record, error)
    After(ctx context.Context, tenant tenancy.ID, object, verb string, rec Record)
}
```

`CRUD` holds an optional `Hooks`; nil means today's behaviour exactly. `kernel/plugins` implements
it, `internal/app` wires it. Consumer-side interface, one implementation, no cycle — the
`kernel/secrets` `Grants` seam in the same shape.

`Before` returns a `Record` because a `before_*` hook that may only veto is half a hook: enrichment
(default a field, normalise a code) is the other half of what ADR-007's `before_validate` is for.
The returned record is re-validated by `CRUD` before the write, so a plugin cannot use enrichment
to write a value the schema forbids (INV-T5).

## 2. Sync hooks fire on CRUD verbs; async hooks fire on anything the feed carries

`before_create`, `before_update`, `before_delete`, and their `after_*` counterparts, on metadata
objects. That is what a choke point inside `CRUD` can honestly offer.

The async side rides the change feed, which carries **every** committed change including
event-sourced ones, so an `after_*` hook sees invoice postings and ledger entries as well as CRUD
rows — a wider surface than the sync side, for free.

**The gap, stated rather than discovered later: the feed has no verb.** `changefeed.Entry` is
`{source, ref_id, object, scope_key}` — it says *an Invoice changed*, not *an Invoice was posted*.
So async hooks bind to `<Object>.changed` and read current state to decide what happened; docs/05's
`invoice.posted` example is **not literally supported by this WP**. Closing it means either a
`verb` column on `change_feed` — an append-only table carrying INV-S5, so a change there is a
careful WP of its own, not a line item here — or dispatch inside the posting pipeline. Recorded in
docs/05 with that choice left open rather than quietly picked.

## 3. A sync hook fails closed by default, and the manifest may say otherwise

ADR-007: "a timing-out `before_commit` hook rejects with a clear error." So a hook that traps,
times out, or is refused a host call **rejects the write**. Anything else means a compliance rule
that silently stops applying the moment the plugin breaks.

That creates the obvious problem: a broken plugin takes writes down. The circuit breaker cannot
resolve it by itself — "breaker open, skip the hook" turns a fail-closed rule into a fail-open one
at exactly the moment the plugin is misbehaving, which is the worst possible time to start ignoring
it.

So **the hook declares which it is**:

```yaml
hooks:
  - {event: Contact.before_create, fn: validate, mode: sync, on_failure: reject}   # default
  - {event: Contact.before_create, fn: enrich,   mode: sync, on_failure: allow}
```

`reject` (the default) is for rules that must hold — the write fails with a problem+json naming the
plugin. `allow` is for enrichment nobody's books depend on — the failure is recorded, the write
proceeds. **The breaker only ever skips `allow` hooks.** A `reject` hook that trips the breaker
keeps rejecting, and the administrator is told which plugin is blocking which object, because a
tenant whose invoicing is down needs a name, not a mystery.

This is one enum with a safe default, not a policy engine. It exists because both behaviours are
genuinely correct for different hooks and picking one for everybody would be wrong half the time.

## 4. The breaker's state lives on the plugin row

In-memory state resets every deploy and every crash, which turns "repeated failures trip a breaker"
into "repeated failures trip a breaker until something restarts" — precisely the case where a
plugin is crashing the process. Counters and `opened_at` go on the `plugins` row, which dispatch
already loads.

Open after N consecutive failures (default 5), half-open after a cooldown (default 5 minutes): the
next call is allowed through and either closes the breaker or re-opens it. Every transition is
audited (INV-T4) and visible in `GET /api/v1/plugins`.

## 5. The async runner is driven by the feed notifier, and delivery is at-least-once

`changefeed.InProcess.Subscribe(tenant)` already rings a bell per tenant; the runner reads from its
per-plugin cursor on that signal, plus a periodic sweep so retries and a cold start make progress
without a write to wake them.

Per-plugin cursors live in a `plugin_deliveries` table, **written at install** rather than created
lazily on the first delivery pass. That was a defect the tests found: a lazily-created cursor is
set to the feed's high-water mark at *first pass*, so everything between the install and that pass
is silently skipped — a gap nobody would think to look for, since the plugin appears to be working.
Recording it at install also gives the intended property for free: a plugin installed today does
not replay the tenant's history, but does see everything from the moment it was approved. **Delivery is at-least-once, as docs/05
promises, and this WP does not pretend otherwise:** two nodes running the runner can both deliver
an entry. The cursor advance is a compare-and-set on the stored value so the window is one entry
rather than a whole page, and hook authors are told plainly that `after_*` must be idempotent —
which is why `kv` ships in this WP, since a dedupe key needs somewhere to live.

A delivery that fails after its retries is filed in `plugin_dead_letters` with the entry, the
error and a timestamp, and is visible over the API. Nothing is dropped silently — the same shape as
the conflict tray's promise under INV-S4, for the same reason.

## 6. A plugin does not react to its own writes

Dispatch is suppressed when the change's actor is the plugin itself (`plugin:<id>` is already the
audit actor from WP-3.1a). Self-suppression rather than a depth counter: the loop is cut at the
source instead of bounded after the fact.

The consequence is deliberate and worth writing down — a plugin genuinely cannot chain off its own
output. A plugin that wants a two-stage pipeline runs both stages in one hook.

## 7. Latency is measured per plugin, not merely capped

The default sync budget is **50ms** (docs/05's 500ms is the ceiling a manifest may raise to, not
the default), and raising it warns the installing administrator in plain language what it costs per
write.

The load-bearing half is attribution: every hook invocation records its duration, and
`GET /api/v1/plugins` reports per-plugin call counts, failures and p95. Without it a slow plugin
makes "the ERP is slow", which is the incumbent failure docs/14 exists to avoid; with it the same
slowness reads as "com.acme.x adds 180ms to every Contact write".

Held in a per-process ring buffer rather than a table: the numbers are for an administrator asking
"what is my plugin costing me", not an audit trail, and a write per hook call to store timings
would be the plugin tax measuring itself. Resets on restart, and the API says so.

## 8. Out of scope, with owners

Settled in the plan review and already refused by name in WP-3.1a's manifest validation:
`/ext/<plugin>/` endpoints → **WP-3.2**; `enqueue_job` and `schedule:` → **WP-3.3**'s job runner;
`emit_event` → unscheduled, because untrusted code writing the event store is INV-E territory that
deserves its own design.
