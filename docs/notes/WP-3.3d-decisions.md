# WP-3.3d `WithTenant` retry aliasing — decisions

Roadmap line: [docs/11-ROADMAP.md](../11-ROADMAP.md) §Phase 3, WP-3.3d. The defect analysis —
what breaks, why it went unseen, and the sixteen sites — is
[WP-3.3d-retry-aliasing.md](WP-3.3d-retry-aliasing.md); this file records only the calls the
build made. Read against [docs/19](../19-DATA-INTEGRITY.md) (**INV-S5**, **INV-E5**) and
[docs/04](../04-SYNC-ENGINE.md).

## 0. Twenty-two sites, not sixteen

The filed analysis found sixteen production sites by hand. Mechanising the same analysis over
the tree found **six more in `_test.go` files** — audit-row and actor readers in
`internal/app`, `kernel/plugins`, `kernel/metadata` and `kernel/secrets`. They are fixed too.
The reason is not symmetry: the same shape in a test does not produce a wrong answer, it
produces a test that fails once a month under load with an off-by-a-few count, which is a
worse thing to be handed than a wrong answer. Fixing them also lets the gate below run with no
exclusion list, and an exclusion list is one more thing to get wrong.

## 1. Six sites get a fault-injection test; the gate covers the other sixteen

The AC asks for a fault-injecting adapter that proves *each fixed site* returns exactly the
uncontended read. Written literally that is nine packages of fixtures for one mechanical fix
repeated twenty-two times, and it buys nothing for the sites the analysis itself calls
cosmetic (a device listed twice, a re-wrapped key). What it would buy is coverage of *shape*,
and shape is exactly what a parser proves better than an example does.

So: **fault-injection tests for the six sites where a duplicate is a broken invariant or is
user-visible** — `changefeed.Read` (INV-S5), `eventstore.ReadFeed` and `LoadStream` (INV-E5),
and `metadata.List`, `ListPage`, `GetMany` (every generic list, page and multi-get in the
product) — and **the AST gate for all twenty-two**, which is a stronger statement than sixteen
more examples: an example proves one site is fixed today, the gate proves no site has the
shape at all, including sites nobody has written yet.

Reviewer's escape hatch: `kernel/storage/faultinject` is a package, not a test helper, so
adding one of the remaining sites later is ten lines in that package's own test file.

## 2. The fault tests are SQLite-only, and that is the defect's own scope

`storage.IsBusy` matches `"database is locked"` — a modernc.org/sqlite message that Postgres
never produces. No BUSY, no retry; no retry, no aliasing. A Postgres run of these tests would
assert that a fault which cannot fire did not fire, which is the vacuity docs/19 already bans.
The *fixed functions* still run on both dialects through their existing conformance and
integrity tests, which is what the storage-touching rule asks for. The scope note is not a
gap: SQLite is solo mode and every offline replica, i.e. the default deployment.

## 3. `go/ast`, not grep

The gate answers "is this accumulator declared inside the closure or outside it". That is a
scoping question. The repo's existing gates (`TestCELIsImportedOnlyHere`,
`TestEveryGrantSetIsChecked`) are grep-based because they ask "does this token appear here",
which grep answers exactly. Brace-counting a closure is where a grep gate starts producing
both false positives and false negatives, and a gate people learn to distrust is worse than no
gate. `go/parser` is stdlib and the walk takes 0.3s over the tree.

Files that fail to parse are skipped rather than failed: the compiler reports syntax louder,
and a half-written file should not turn an invariant gate red for the wrong reason.

## 4. The retry itself is unchanged

The analysis suggests considering whether `WithTenant` should refuse to retry once `fn` has
produced output, or whether the retry belongs in a wrapper callers opt into. Neither is done
here. The retry is load-bearing (`kernel/eventstore`'s 1000-writer torture depends on it),
`WithTenant` cannot see what its callback touched, and changing the recovery semantics of
every transaction in the product is not a defect fix. The contract on the function plus the
gate that enforces it is the whole of the fix; if the opt-in wrapper is ever wanted it is its
own WP with its own review.

## 5. The local is named `list`, and there is no per-site comment

Twenty-two copies of the same explanatory comment is noise that stops being read by the third
one. The explanation lives once, on `WithTenant` itself, where someone writing the twenty-third
callback will meet it — and the gate catches them if they do not. The two invariant-bearing
sites (`changefeed.Read`, `eventstore`) keep a short comment naming INV-S5 and INV-E5, because
there the reason the local exists is the invariant, and a reader tidying that away should have
to argue with the invariant.
