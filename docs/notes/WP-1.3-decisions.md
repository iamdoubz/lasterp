# WP-1.3 Tax engine v1 — decisions

Interpretation of an underspecified WP, per CLAUDE.md "resolve ambiguity in
writing." ADR-013 is the governing decision record; nothing here relitigates
it.

## Scope (what v1 is / is not)

The WP line: *"jurisdictions, rules, effective-dated rates as data; document tax
calculation; US state + EU VAT seed packs. AC: golden-file test suite of tax
scenarios."*

**In:** effective-dated tax-rate store, jurisdiction reference data, a pure
document tax calculation (exclusive + inclusive, per-rule rounding), embedded US
+ EU seed packs, and a golden-file suite.

**Out (deferred, flagged):**
- **Place-of-supply / ship-from→ship-to resolution engine.** ADR-013 mentions
  "jurisdiction resolution chain ... as data." v1 takes an already-resolved
  `(jurisdiction, category)` per line; the caller (invoicing WP-1.4) resolves it.
  A rules engine that *derives* the jurisdiction from addresses is a large,
  separate concern — not needed to price a document whose jurisdiction is known.
- **HTTP/MCP surface.** Like `fx_rates` (WP-1.1), the tax reference tables and
  calc are a Go API in v1; REST/MCP exposure lands with the web client (WP-1.5) /
  MCP (WP-3.4), the same phase-1 precedent the ledger's `Post`/`Reverse` follow.
  Commandment 2 is satisfied at the vertical-slice level when invoicing exposes it.
- **Full 50-state / 27-country data.** Seed packs ship a representative,
  disclaimered subset (ADR-013: "packs carry disclaimers, not legal advice";
  "community-maintained seed packs"). Breadth is a data PR, not engine work.
- **Compound / tax-on-tax, withholding, reverse-charge, per-document (vs
  per-line) rounding aggregation.** v1 rounds per line and sums; the golden
  format is designed so these extend as new scenarios without an engine rewrite.

## Data model — raw reference tables, mirroring `fx_rates`

Two tables, both following the WP-1.1 `fx_rates` pattern exactly (global sentinel
`tenant_id = ''` for provider/seed rows, tenant rows override, RLS **split**
policies so a tenant can read global+own but write only own):

- **`tax_jurisdictions`** `(tenant_id, code, name, country, level, recorded_at)`.
  `level ∈ {country, state, province}`. Static reference (no effective-dating).
- **`tax_rates`** `(tenant_id, jurisdiction, category, rate, rounding, as_of,
  name, provider, recorded_at)`. `rate` is an exact decimal string fraction
  (`"0.20"` = 20 %); `category ∈ {standard, reduced, zero, exempt, sales, ...}`
  (open string — packs define their own); `rounding ∈ {half_even, half_up}` is
  the **"rule as data"** (some jurisdictions mandate half-up); `as_of` is a plain
  `YYYY-MM-DD` effective-from string (sorts/compares identically PG+SQLite, the
  fx lesson). Lookup = latest `as_of <= date`, tenant override beats global.

**Why raw tables, not metadata CRUD objects:** `fx_rates` set the precedent that
effective-dated *reference data* is a raw table + Go API, not a `metadata.CRUD`
object. It keeps global-sentinel seeding trivial (write under `WithTenant('')`,
no authz actor to invent for shared rows) and effective-dating uniform with fx.
A metadata-CRUD jurisdiction would force an authorizing actor for seed writes
under the `''` sentinel, which is nonsensical. Isolation is still DB-enforced
(RLS), so INV-T1 holds.

**No authz on rate/jurisdiction writes in v1** — same as `fx_rates`. These are
reference-data seeding paths, not tenant business writes. When the API surface
lands (WP-1.4/1.5) it adds authz at that seam. INV-T2/T4 are about business-write
paths; reference seeding under the global sentinel is analogous to fx.

## Calculation — pure, separate from lookup

- `Calculate(Document) Result` is **pure** (no DB): each `Line{Base money.Money,
  Rate string, Rounding, Inclusive}` → `LineResult{Net, Tax}`, plus totals
  grouped per `(jurisdiction, category)`. This is the golden-file target and what
  invoicing calls after resolving rates.
  - **Exclusive** (US sales, EU B2B): `tax = round(base × rate)`, `net = base`.
  - **Inclusive** (EU B2C gross prices): `net = round(base / (1+rate))`,
    `tax = base − net`. Extraction, so the cent is conserved (`net + tax = gross`).
- `ResolveAndCalculate(ctx, db, tenant, ..., date)` does the DB rate lookup, then
  calls the pure `Calculate`. DB-backed; tested in the integration suite.

All money math goes through **one new `kernel/money` helper**,
`Money.MulRat(ratio *big.Rat, mode RoundingMode) (Money, error)` — same-currency
`round(amount × ratio)`, exact `math/big`, no float. `Money`'s fields are
unexported, so the tax package *cannot* do this arithmetic itself; the helper
keeps INV-F4 (the "no floats, rounding only via kernel/money" rule) intact. It's
the same-currency sibling of the existing cross-currency `Convert`.

## Invariants (INV-*) touched

No **new** catalog invariant. Tax reuses two already `TestRequired`:

- **INV-F4** (money is integer minor units + ISO-4217; no floats; conserves every
  cent). The tax calc is a money path: exact `big.Rat`, rounding only via
  `money.MulRat`, and inclusive extraction proven cent-conserving
  (`net + tax = gross`). Certified by the **golden-file suite** (docs/19's
  "Golden files: tax … calculations vs certified expected outputs").
- **INV-T1** (no cross-tenant rows; RLS backstop). `tax_rates` /
  `tax_jurisdictions` get the fx-style split RLS; the integration suite proves a
  tenant reads global+own only and cannot read/write another tenant's rows, on
  Postgres AND SQLite.

docs/19 treats tax under the **Golden-files** gauntlet suite, not a numbered
INV-F; that is the integrity contribution here, alongside INV-F4/T1 reuse. Both
new tables are appended to no append-only set (they are mutable reference data —
rates get corrected/re-seeded; history is the effective-dating, not immutability).
