# WP-3.3d — `WithTenant` retries the callback; sixteen callbacks are not idempotent

**Found 2026-08-31, during WP-3.3b.** Filed as its own WP because the fix spans nine packages
including `kernel/changefeed` and `kernel/eventstore`, and because the reasoning is worth
having written down before someone "tidies" the fix away.

## 1. The defect

`tenancy.WithTenant` retries **the entire callback** when a statement fails with
`SQLITE_BUSY`:

```go
for attempt := 0; ; attempt++ {
    err = withTenantOnce(ctx, db, tenant, fn)   // fn runs again, from the top
    if err == nil || !storage.IsBusy(err) { return err }
    ...
    time.Sleep(BusyBackoff(attempt))
}
```

That is correct on its own terms — the transaction rolled back, so re-running it is the right
recovery — and it is load-bearing: `kernel/eventstore`'s 1000-writer torture depends on it.

The problem is the contract it implies and never states. **A retried callback must be
idempotent with respect to everything it touches outside the transaction**, and sixteen
callbacks are not: they append result rows into a slice declared in the *enclosing* function.
A retry after a partially-consumed `rows` therefore returns the first attempt's rows *plus* the
second's.

```go
var out []Thing                                    // ← enclosing scope
err := tenancy.WithTenant(ctx, db, tenant, func(...) error {
    rows, err := tx.QueryContext(...)              // ← may fail mid-scan on BUSY
    for rows.Next() { out = append(out, ...) }     // ← survives the rollback
    return rows.Err()
})
```

Nothing rolls `out` back. The database is consistent; the value the caller receives is not.

## 2. Why it has never been seen

- **SQLite only.** `storage.IsBusy` matches `"database is locked"`; Postgres never produces it,
  so the retry loop never runs there. The exposure is solo mode and the client replica —
  which is to say, the default deployment and every offline client.
- **It needs read contention**, and it needs the BUSY to land *after* the scan has already
  appended at least one row. Most reads are small and most deployments are single-user.
- **The result is plausible.** Duplicated rows in a list read like data, not like a fault. There
  is no error, no log line, and no assertion anywhere that a list has no duplicates.

## 3. How it was found

Not by review. WP-3.3b's `jobs.Claim` returned a job to *three* workers at once, and the SQL
was rewritten twice chasing it before instrumentation showed the claim was never the problem:

1. An attempt claimed job X, set the captured `claimed = &X`, and its commit failed BUSY.
2. `WithTenant` re-ran the callback. Another worker had taken X, so this attempt found the
   queue empty and returned `nil` — **without clearing `claimed`**.
3. `WithTenant` reported success, and `Claim` handed back a job this worker never held and
   another worker was already running.

The read-only form is the same bug with a slice instead of a pointer. `jobs.Claim`,
`jobs.DeadLetters`, `jobs.ListSchedules`, `automations.*` and `authz.GrantsFor` were written or
corrected against it during WP-3.3b; everything below predates it.

## 4. The affected sites

Sixteen, all confirmed by locating the accumulator's declaration relative to the closure. Write
paths that build `cols`/`vals`/`setClauses`/`args` are **not** affected — those are declared
inside their closure and are rebuilt per attempt.

| Site | What duplicates | Why it matters |
|---|---|---|
| `kernel/changefeed/feed.go:156` | `changes` in `Read` | **The one to fix first.** INV-S5 promises a reader observes every committed entry *exactly once*. A duplicated entry is that promise broken, and every downstream consumer — the replica, the plugin hook runner, the automation runner — trusts it. |
| `kernel/eventstore/feed.go:46, 93` | `events` | A projection folding a duplicated event double-counts. INV-E5 says `rebuild(events) ≡ projection`; this is a way for the *same* log to fold differently on two reads. Money paths are downstream. |
| `kernel/metadata/crud.go:312, 364, 396` | `records` in `ListPage`, `GetMany`, `List` | Duplicated rows in every generic list, page and multi-get — the widest blast radius, and the one a user would see. |
| `kernel/authz/rbac.go:189, 246` | `objects` in `GrantedObjects`, `names` in `RolesFor` | Duplicates only, no widening: the set is the same, so no access changes. Cosmetic, but it feeds WP-2.4's sync scope. |
| `kernel/identity/device.go:104` | `devices` | A device listed twice in the UI. |
| `kernel/secrets/secrets.go:241` | `out` in the metadata listing | Names only; no values are returned by any route (INV-K1). |
| `kernel/secrets/rotate.go:54` | `stale` | A key re-wrapped twice. Idempotent by construction, so cosmetic. |
| `kernel/plugins/install.go:342` | `out` in `List` | Plugins listed twice. |
| `kernel/plugins/runner.go:276` | `out` in `DeadLetters` | Dead letters listed twice. |
| `modules/reporting/accounts.go:41` | `out` | **A report row counted twice.** Cosmetic only if nothing sums it. |
| `modules/tax/jurisdictions.go:78` | `out` | A jurisdiction listed twice. |
| `internal/app/conformance.go:165` | `out` | Test/conformance surface. |

## 5. The fix

Two shapes, and the second is what makes it stay fixed.

**Per site:** build into a local, assign once at the end. The local is re-created on every
attempt, so a retry starts from nothing.

```go
var out []Thing
err := tenancy.WithTenant(ctx, db, tenant, func(...) error {
    var list []Thing                  // ← per attempt
    for rows.Next() { list = append(list, ...) }
    if err := rows.Err(); err != nil { return err }
    out = list                        // ← once, after the last thing that can fail
    return nil
})
```

**Structurally:** state the contract on `WithTenant` itself — *the callback may run more than
once; it must not accumulate into anything it captures* — and add a gate that enforces it. A
grep-based check in the style of `TestEveryGrantSetIsChecked` and `TestCELIsImportedOnlyHere`
is enough: flag any `X = append(X, …)` inside a `WithTenant` closure whose `X` is declared
outside it. That is exactly the analysis §4 was built from, so it is known to be mechanisable,
and it is the half that prevents the sixteenth reintroduction.

Consider also whether `WithTenant` should refuse to retry at all once `fn` has produced output
— it cannot know — or whether the retry belongs in a wrapper a caller opts into. Both are
bigger changes than the aliasing fix and should not block it.

## 6. Testing

The property is "a retried read returns what one read returns". A fault-injecting
`storage.DB` wrapper that fails the *second* `rows.Next()` with `database is locked` on the
first attempt and succeeds on the second makes it directly assertable, per site, without
contention or timing:

- the returned slice equals the uncontended read, exactly — not merely "contains";
- the injected fault is asserted to have fired, or the test is measuring nothing (the
  non-vacuity rule docs/19 already applies to the sync harness);
- `changefeed.Read` gets the INV-S5 wording as its assertion: every committed entry observed
  exactly once, across a retry.

Mutation check: revert one site to the captured-slice form and the matching test must go red.

## 7. Scope note

This is not an automations or a jobs defect — WP-3.3b only found it. It is filed here rather
than fixed in that PR because it touches `kernel/changefeed` and `kernel/eventstore`, both
under CODEOWNERS, and because a sixteen-site change buried in a feature branch is one nobody
reviews as the invariant question it is.
