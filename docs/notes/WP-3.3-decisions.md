# WP-3.3 Automations — decisions

Roadmap line: [docs/11-ROADMAP.md](../11-ROADMAP.md) §Phase 3, WP-3.3. Read against
[docs/08](../08-SECURITY-MULTITENANCY.md) §AuthZ, [docs/05](../05-PLUGIN-SYSTEM.md) §host
functions, [ADR-006](../adr/ADR-006-metadata-customization.md), [ADR-007](../adr/ADR-007-plugin-system.md),
[ADR-014](../adr/ADR-014-self-evolution-governance.md) and [docs/19](../19-DATA-INTEGRITY.md).

## 0. The line is three things, and they ship as two PRs — 3.3a and 3.3b

The roadmap line carries three separable pieces: an expression language activated on the
**authorization** path, a general-purpose **job runner**, and **automations** as metadata.
They share a dependency (automations need both) and nothing else: one is a security surface,
one is scheduling infrastructure, one is a product feature built on the other two. The repo's
own precedent for a line this shape is a split — WP-1.6, WP-2.2, WP-2.3, WP-3.1 and WP-3.2 all
became two PRs after a plan review, each time for this reason.

- **WP-3.3a — the expression engine and conditional grants.** `kernel/expr` (the CEL
  environment and the compile/evaluate seam) and `kernel/authz` condition evaluation, which
  has been stored-but-unevaluated since WP-0.3. Carries **INV-T3**. Ships
  [ADR-022](../adr/ADR-022-expression-language.md).
- **WP-3.3b — the job runner and automations.** `kernel/jobs` (durable queue, cron schedules,
  retries, dead letters), the plugin manifest's `schedule:` capability and the `enqueue_job`
  host function that WP-3.1a refused by name, and `kernel/automations` (trigger → condition →
  action, stored as metadata, driven off the change feed). Carries **INV-X1/X2** and **INV-T4**
  through the new job path, and reuses 3.3a's evaluator for `condition:`.

3.3a is a prerequisite of 3.3b, not a parallel half; it goes first.

## 1. CEL is a dependency, not a reimplementation (ADR-022)

docs/08 names CEL by name — "condition is an optional CEL expression over record + actor" —
and the roadmap line repeats it. The alternative was a hand-written evaluator over the subset
those docs actually demonstrate (`record.owner == actor.id || actor.team in record.team`),
which is roughly 250 lines and no new modules.

We take the dependency (`cel.dev/cel-go`), for reasons narrow enough to state:

1. **This is the authorization path.** A hand-rolled parser is exactly where an operator
   precedence or type-coercion bug becomes a permission *widening*, which is the failure
   INV-T3 exists to prevent. cel-go is the reference implementation of the language the docs
   promise, and the same evaluator Kubernetes admission and Envoy RBAC use for the same job.
2. **"CEL-shaped" is worse than either option.** A subset evaluator that accepts CEL syntax and
   diverges on semantics is a trap for the administrator writing the condition and for the
   agent authoring one under docs/13 — they read the CEL spec and get something else.
3. The cost is honest and bounded: five new modules (`cel.dev/expr`, `antlr4-go/antlr/v4`,
   `golang.org/x/exp`, two `genproto` leaves) plus a `protobuf` bump. No CGO, no service, no
   second toolchain.

Recorded as ADR-022 because CLAUDE.md requires an ADR for a new heavyweight runtime library,
and because "we do not write our own expression language" is the part worth being able to cite
later.

## 2. INV-T3 is structural: a condition is an AND, never an OR

The narrowing property is not a claim about what CEL computes. It is a property of *where the
evaluation sits*:

- `Can`/`Authorize` first find the grants for `(object, action)`. No grant → denied, exactly as
  today. A condition is only ever consulted **after** a matching grant has been found, so no
  condition can produce an allow where no grant exists.
- A condition that evaluates false, errors, returns a non-boolean, fails to compile, or exceeds
  its evaluation budget **denies that grant**. Fail-closed in every direction; there is no path
  where an unevaluable condition degrades to "allow".

So for any grant set G, `allowed(G with conditions) ⊆ allowed(G with every condition removed)`.
That containment is what the property test asserts, over generated conditions including
malformed and hostile ones — and it holds without trusting the evaluator's semantics at all.

## 3. A conditional grant does not satisfy a record-less gate

`Authorize(ctx, db, object, action)` has no record to judge, so a condition over `record.*`
cannot be evaluated there. Treating a conditional grant as sufficient at that gate is precisely
the widening INV-T3 forbids, and it is the shape the bug would actually take.

Therefore:

- `Can`/`Authorize` (record-less) are satisfied **only by an unconditional grant**. Today's
  behaviour, unchanged, for every existing caller.
- A new `AuthorizeRecord(ctx, db, object, action, rec)` is satisfied by an unconditional grant
  **or** by a conditional grant whose expression evaluates true against `{record, actor}`. This
  is the gate record-bearing writes and `Get` move to.
- `GrantedObjects` (WP-2.4's sync scope) keeps returning unconditionally-granted objects only.
  Including a conditionally-granted object there would hand a replica rows the condition denies
  — INV-T1/T2 through the sync door.

**Named deferral:** *row-level filtering of list and sync reads under a conditional read grant.*
A condition on a list path has to be pushed into the query or applied per row, with a decision
about pagination under a filter that rejects; that is a WP, not a paragraph here. Until it
lands, `GrantPermission` **refuses** a condition on the `read` action with that WP named in the
error — the same "refuse rather than silently ignore" the plugin manifest already uses for
`schedule:`. An administrator is told which WP they are waiting on instead of installing a
row-level rule that quietly filters nothing.

## 4. The evaluation environment is closed

`record` and `actor` are the only bindings, and `actor` exposes `id`, `tenant` and `roles` —
nothing else. No extension functions, no access to host state, no clock, no network. An
expression is compiled at grant time (a grant whose expression does not compile is refused, not
stored) and served from a process-level compiled cache at evaluation. Evaluation carries a cost
budget, so a pathological expression is a denied grant rather than a stalled request.

`actor.roles` is included because docs/08's own example needs a fact about the actor beyond the
id, and because role names are already readable through `RolesFor`. Role names are not an
authorization primitive there and are not one here: a condition can only narrow.

## 4b. Three things building 3.3a found

**The containment property is vacuous unless the grid includes ungranted cells.** The first
version of `TestConditionCanOnlyNarrowAGrant` probed each arm only where it held a grant. The
unconditional twin then allows everything, so `allowed(with condition) ⟹ allowed(without)` is
satisfied by construction and the test asserts nothing. The fix is the probe grid: four objects
× three actions against a role granted two or three of them, so most cells are ungranted and a
condition that reaches past its own `(object, action)` — which is the shape a widening bug
actually takes — lands where the twin denies. Non-vacuity is now asserted three ways: some cell
narrowed, some allowed by both, some denied by both.

**INV-T3 needs two tests, and each was mutation-checked to prove the other cannot replace it.**
Making an errored condition allow (`err != nil → true`) leaves the containment property *green*
— the conditional arm allows more, but only where it holds a grant, and there the twin allows
too. It turns the fail-closed table red. Conversely, leaking a conditional grant across actions
turns the property red and the table green. The catalog entry records the pair rather than
either test, because a future reader tempted to delete "the redundant one" is deleting half the
invariant.

**A condition is `USING`, not `WITH CHECK`.** On update it is evaluated against the *current*
row, inside the transaction that will write — so it says which rows you may touch, not what
they may become. A rule stopping an owner from reassigning their record to someone else is a
validation rule or a hook today. Marked in `crud.go` with the upgrade path rather than left for
someone to discover from behaviour.

## 5. Automations run as their own principal, through the ordinary pipeline (3.3b)

An automation is `automation:<id>`, never the user whose write triggered it — the same call
WP-3.1a made for plugins, for the same reason: an audit row must name what acted, and authority
that varies by trigger cannot be reviewed when the automation is authored. Its actions execute
through the same CRUD, the same authorization gate, the same validation and the same audit
(INV-T2/T4, and INV-X3's rule in advance of WP-3.4). An automation does not react to its own
writes, by the actor-suppression WP-3.1b already built into the feed runner.

**Actions shipped in 3.3b:** `field_update` (through `CRUD.Update`, re-validated — INV-T5),
`webhook` (through WP-3.2a's audited outbound client, per-automation allowlist), and
`call_plugin` (an enqueued job invoking a declared plugin function).

**Named deferral:** *`email` and `approval_request` actions.* docs/05's table lists both. There
is no mailer in the tree and no approval object; building either as a line item inside an
automations WP is how an action surface acquires a shape nobody designed. `email` lands with
the WP that ships outbound mail; `approval_request` with the approval-gate work WP-3.4 needs
anyway. An automation declaring either is refused at parse time, naming the owner.

## 6. The job runner is a table, not a broker (3.3b)

Solo mode is one binary (ADR-011). `kernel/jobs` is a tenant-scoped table claimed by an atomic
conditional `UPDATE` — not `SELECT … FOR UPDATE SKIP LOCKED`, which SQLite does not have and
which the storage adapter would therefore have to fork. Retries are bounded with backoff, and a
failure that exhausts them is filed as a dead letter while the queue moves on, mirroring
`plugin_dead_letters` exactly: a queue head that retries forever blocks everything behind it,
which is the stall INV-S4 counts as a silent drop.

Cron is a hand-written 5-field parser (~100 lines, table-driven test), not a dependency. This
is the opposite call from §1, deliberately: cron has no security surface and no ambiguity in
the five-field form, and a wrong answer produces a job at the wrong minute rather than an
authorization decision for the wrong actor.
