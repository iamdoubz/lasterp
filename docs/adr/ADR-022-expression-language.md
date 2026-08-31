# ADR-022: Conditional grants and automation conditions use cel-go, not a hand-written evaluator

**Status:** Accepted · 2026-08-31

## Context

WP-3.3a activates the condition half of RBAC. `role_permissions.condition` has been stored and
never evaluated since WP-0.3, which refused a conditional grant outright rather than silently
honouring it unconditionally (`authz.ErrConditionNotSupported`). docs/08 §AuthZ names the
language: "condition is an optional CEL expression over record + actor (e.g.,
`record.owner == actor.id || actor.team in record.team`)". The WP-3.3 roadmap line repeats it,
and WP-3.3b's automations need the same evaluator for their `condition:`.

CLAUDE.md forbids a new heavyweight runtime dependency without an ADR, so the choice is a
deliverable. The candidates were `cel.dev/cel-go` (the reference CEL implementation), one of
the general-purpose Go expression evaluators (`expr-lang/expr`, `antonmedv/govaluate` and kin),
or a hand-written evaluator over the subset docs/08 demonstrates.

[ADR-019](ADR-019-jose.md) is the standing precedent and it went the other way: it declined a
JOSE library and verified ID tokens over stdlib crypto. That ADR is right and this one does not
weaken it — the difference is stated below.

## Decision

**Take `cel.dev/cel-go`.** `kernel/expr` wraps it in a closed environment and is the only place
in the tree that imports it.

1. **The environment is closed.** Two bindings — `record` (the object's field map) and `actor`
   (`id`, `tenant`, `roles`). No extension functions, no host state, no clock, no network, no
   protobuf message types. An expression cannot reach anything the caller did not hand it.
2. **Expressions are compiled at grant time.** A condition that does not compile, or whose
   result type is not `bool`, is refused by `GrantPermission` and never stored. Compiled
   programs are cached per process by expression text.
3. **Evaluation is fail-closed and budgeted.** False, error, non-boolean result, or cost limit
   exceeded — each denies the grant being evaluated. There is no path where an unevaluable
   condition degrades to "allow".
4. **A condition can only narrow.** It is consulted after a matching grant is found, never
   instead of one, so it is structurally an AND (docs/notes/WP-3.3-decisions.md §2). This is
   what carries INV-T3 and it does not depend on cel-go being semantically correct.

Cost: five new modules (`cel.dev/expr`, `github.com/antlr4-go/antlr/v4`, `golang.org/x/exp`,
`google.golang.org/genproto/googleapis/{api,rpc}`) plus a `google.golang.org/protobuf` bump.
Pure Go, no CGO, no service, no second toolchain — the constraints ADR-001 and ADR-011 impose
are untouched.

## Rationale

**Why not hand-written, when ADR-019 was?** ADR-019 declined a library because the hard part —
RSA/ECDSA verification — was *already in stdlib*, so the library would have added surface
around primitives we would use anyway, and the dangerous parts (`alg: none`, HMAC confusion)
were removed by making them unrepresentable. Neither holds here. There is no expression
evaluator in stdlib, so "hand-written" means we own a lexer, a parser, a precedence table and a
coercion model — on the authorization path, which is exactly where a precedence or coercion bug
becomes a permission *widening*. The subset is not small enough to be obviously correct and not
big enough to be worth owning.

**Why not a "CEL-shaped" subset?** Worse than either option. An evaluator that accepts CEL
syntax and diverges on semantics is a trap for the administrator writing a condition and for
the agent authoring one under docs/13 — both read the CEL spec and get something else. If we
were not going to implement CEL we would have to amend docs/08 to stop promising it.

**Why cel-go over `expr-lang/expr`?** The docs promise CEL specifically, and CEL is the
language with a specification, a conformance suite, and an existing audience: it is what
Kubernetes admission policy and Envoy RBAC evaluate, for the same "narrow a permission with a
predicate" job. A tenant admin's condition is portable knowledge rather than a LastERP dialect.

**Why the cost is acceptable.** ANTLR's runtime is the bulk of it and it is parse-time only.
Compilation happens at grant time and is cached; the request path evaluates an already-compiled
program under a cost budget.

## Rejected

- **Hand-written subset evaluator** — a parser we own on the permission path, and a documented
  language we would then only partly implement.
- **`expr-lang/expr` or another general evaluator** — a comparable dependency for a language
  the docs do not name, trading a specification for nothing.
- **Storing conditions and continuing to refuse them** (the WP-0.3 status quo) — honest, but
  the roadmap line and docs/08 both promise the feature, and row-level rules are the most
  requested customization an ERP admin has.

## Consequences

- `kernel/expr` is the single import site for cel-go; nothing else in the tree may import it,
  so the environment cannot be widened from a call site. A test asserts that.
- Conditional grants are refused on the `read` action until row-level filtering of list and
  sync reads lands, because `GrantedObjects` and the sync scope answer object-level questions
  and would otherwise widen a replica's contents (WP-3.3-decisions.md §3).
- WP-3.3b's automation `condition:` reuses `kernel/expr` unchanged. WP-3.4's MCP tools and
  docs/13's agent-authored customizations inherit the same closed environment rather than each
  inventing a predicate language.
- The dependency joins the supply-chain surface WP-1.10 hardened; it is pinned in `go.mod` and
  covered by the existing `govulncheck` gate.
