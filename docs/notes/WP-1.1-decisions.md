# WP-1.1 decisions & scope — Money & currency core

**Status: unblocked, planned (2026-07-17).** First financial-primitive WP; the
foundation WP-1.2 (ledger), WP-1.3 (tax), WP-1.4 (invoicing) all build on.

## Dependency check
`kernel/money` is kernel infrastructure, not a `modules/*` WP, so the WP-0.8 gate
("never start a module WP before WP-0.8 is green") does not gate it — and WP-0.8 is
merged regardless. Prerequisites — WP-0.2 storage, WP-0.3 tenancy (RLS), WP-0.8
integrity catalog — are all in `main`. No lateral module imports.

## Invariants this WP touches (docs/19)
- **INV-F4** (lands here) — "Money is integer minor units + ISO-4217; no floats
  anywhere in a money path; allocation never creates or destroys a cent
  (Σparts = whole)." This WP flips `INV-F4.TestRequired = true` in the catalog and
  adds tagged property tests. Enforcement is **two-layer**: (1) the type system —
  `Money.amount` is an unexported `int64`, so a float amount is unrepresentable; no
  money constructor or op accepts/returns a float; (2) the allocation algorithm
  conserves every minor unit by construction, proven by a conservation property
  test.
- **INV-T1** — `fx_rates` is tenant-scoped with RLS + FORCE like every other table;
  the effective-dated lookup runs through `tenancy.WithTenant`. The FX store test is
  integrity-tagged and references INV-T1.

## Ambiguities resolved

**1. Money representation = `int64` minor units + ISO-4217 code (settled by
CLAUDE.md, not re-decided here).** `docs/03` left "shopspring/decimal or int64
minor units; WP-1.1 decides." **Decision: `int64` minor units for amounts.** No
`shopspring/decimal` — that would be a new runtime dependency needing an ADR
(CLAUDE.md), and integer minor units are exactly what the commandment mandates.
`Money{amount int64, currency string}` with **unexported** fields so invalid/float
states are unrepresentable outside the package's validated constructors.

**2. Currency registry = a thin wrapper over `golang.org/x/text/currency`, already a
direct dependency.** ISO-4217 minor-unit exponents (USD=2, JPY=0, BHD=3, …) are
standard data; `currency.ParseISO` validates a code and `Standard.Rounding(unit)`
returns the minor-unit scale. Ponytail ladder rung 4 (use the installed dep): no
hand-maintained ISO-4217 table, no new dependency, no ADR. `money.Lookup(code)`
returns `{Code, Digits}`. Cash-rounding increments (CHF 0.05) are available via
`Cash.Rounding` but not wired in v1 (no AC need; flagged).

**3. Rates & non-minor-unit math use `math/big` (stdlib), never float.** FX rates
(1.0873) and conversion multipliers aren't minor units and need exact fractional
math. **Decision:** store rates as exact decimal strings, parse to `*big.Rat`
(`math/big`), do conversion arithmetic in `big.Rat`, and round the result to the
target currency's minor unit via an explicit `RoundingMode`. Allocation uses
`big.Int` for the `amount × ratio` product to avoid `int64` overflow, integer-only.
No float anywhere in a money path (INV-F4).

**4. Rounding modes: `HalfEven` (default) + `HalfUp`.** Half-even (banker's) is the
least-biased default for repeated conversions/allocations; half-up is offered
because some tax jurisdictions mandate it — the tax engine (WP-1.3) selects per
rule. Two modes only; more can be added when a rule needs them.

**5. Allocation = largest-remainder method, deterministic.** `m.Allocate(ratios)`
floors each part to `amount×ratioᵢ/Σratio`, then distributes the leftover minor
units one each to the parts with the largest remainders (tie-break: lower index
first). Σparts == m exactly (INV-F4). Negative amounts are allocated by magnitude
then sign-applied, so conservation holds for both signs. `AllocateEqual(n)` is
`Allocate([1,1,…])`.

**6. FX rate store: tenant-scoped table + effective-dated as-of lookup; global rates
via the sentinel-tenant pattern already used by `object_schemas`.** ADR-013: rates
are "stored historically … overridable per tenant." **Decision:** `fx_rates`
(tenant_id, base, quote, rate, as_of, provider, recorded_at) with RLS + FORCE (the
tenancy commandment). Provider/global rates are stored under the shared sentinel
tenant (`""`, as `kernel/metadata` core schemas do) and the RLS policy admits
`tenant_id = current OR tenant_id = ''`; a per-tenant manual override uses the real
tenant_id. `RateAsOf(base, quote, date)` returns the latest rate with `as_of ≤ date`,
preferring a tenant override over the global rate. v1 supports **direct pair +
identity (base==quote → 1) + inverse (1/rate)**; cross-rate via a pivot currency is
deferred (no AC need).

**7. ECB provider = pure XML parse + thin HTTP fetch.** ECB publishes EUR-based daily
reference rates as XML. **Decision:** `ParseECB(io.Reader) ([]Rate, error)` is a pure
`encoding/xml` parser (stdlib), tested against a fixture and **fuzzed** (CLAUDE.md:
all parsers get fuzz — malformed input is rejected, never panics/corrupts).
`ECBProvider.Rates(ctx)` does an `http.Get` + `ParseECB`; the network fetch is not
exercised in CI (no outbound network in tests). Manual rate entry is just `SaveRate`.

**8. `Money` JSON marshaling = `{"amount": <int64>, "currency": "USD"}`.** Money
travels in event payloads (WP-1.2 ledger) and API bodies; a stable, float-free JSON
shape is provided now so downstream WPs serialize consistently.

**9. Still out of scope (flagged, not silently dropped):**
- A first-class two-column money representation in the **metadata engine**
  (`FieldMoney` is TEXT/JSON today). The metadata engine does no money *arithmetic*;
  the ledger stores money in event payloads via `Money`'s JSON shape. Upgrading the
  metadata column is a separate change with no WP-1.1 AC bearing.
- Cash-rounding increments (CHF 0.05), cross-rate pivoting, realized/unrealized FX
  gain-loss routines (a ledger concern, WP-1.2/M1), and per-tenant currency
  *enablement* lists.
